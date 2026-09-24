"""文件分类与"可点开"副本的生成。

原文件永远走 sendDocument；这里负责产出一个 Telegram 能内联展示的版本：
图片 → JPEG（sendPhoto）、动图 → sendAnimation、视频 → MP4（sendVideo）。

转换工具按"能用就用，不能用就降级"的顺序探测：
Pillow（图片，最省事）→ ffmpeg → ImageMagick。都没有时返回 None，只发原文件。
"""

from __future__ import annotations

import logging
import os
import shutil
import subprocess
import tempfile
from dataclasses import dataclass

log = logging.getLogger(__name__)

IMAGE_EXTS = {
    ".jpg", ".jpeg", ".jfif", ".png", ".gif", ".bmp", ".webp",
    ".tif", ".tiff", ".heic", ".heif", ".avif", ".ico",
}
VIDEO_EXTS = {
    ".mp4", ".m4v", ".mov", ".mkv", ".avi", ".webm", ".flv", ".wmv",
    ".mpg", ".mpeg", ".3gp", ".3g2", ".ts", ".m2ts", ".ogv", ".rmvb", ".vob",
}
PHOTO_PASSTHROUGH_EXTS = {".jpg", ".jpeg", ".jfif", ".png"}
VIDEO_PASSTHROUGH_EXTS = {".mp4", ".m4v", ".mov", ".webm"}
ANIMATION_PASSTHROUGH_EXTS = {".gif", ".mp4"}

CONVERT_TIMEOUT = 3600
MAX_PHOTO_SIDE = 2560
MAX_VIDEO_SIDE = 1280

try:  # pragma: no cover - 取决于运行环境
    from PIL import Image  # type: ignore

    HAVE_PILLOW = True
except ImportError:  # pragma: no cover
    Image = None  # type: ignore
    HAVE_PILLOW = False


def classify(path: str) -> str | None:
    """按扩展名判断是 image / video，其它返回 None。"""
    ext = os.path.splitext(path)[1].lower()
    if ext in IMAGE_EXTS:
        return "image"
    if ext in VIDEO_EXTS:
        return "video"
    return None


@dataclass
class PreparedInline:
    kind: str  # photo / video / animation
    path: str
    temporary: bool
    note: str = ""


class MediaPreparer:
    def __init__(
        self,
        photo_max_bytes: int,
        video_max_bytes: int,
        workdir: str | None = None,
        ffmpeg: str | None = None,
        magick: str | None = None,
        use_pillow: bool = True,
    ) -> None:
        self.photo_max_bytes = photo_max_bytes
        self.video_max_bytes = video_max_bytes
        self._workdir = workdir
        self._owns_workdir = workdir is None
        self._ffmpeg = ffmpeg if ffmpeg is not None else shutil.which("ffmpeg")
        self._magick = magick if magick is not None else (
            shutil.which("magick") or shutil.which("convert")
        )
        self._pillow = HAVE_PILLOW and use_pillow

    @property
    def workdir(self) -> str:
        if self._workdir is None:
            self._workdir = tempfile.mkdtemp(prefix="filebot-")
        return self._workdir

    def close(self) -> None:
        if self._owns_workdir and self._workdir and os.path.isdir(self._workdir):
            shutil.rmtree(self._workdir, ignore_errors=True)
        self._workdir = None

    def capabilities(self) -> str:
        tools = []
        if self._pillow:
            tools.append("pillow")
        if self._ffmpeg:
            tools.append("ffmpeg")
        if self._magick:
            tools.append(os.path.basename(self._magick))
        return ", ".join(tools) or "无（只会发送原文件）"

    # ------------------------------------------------------------------ #
    def prepare(self, path: str, kind: str) -> PreparedInline | None:
        """生成可内联展示的副本；无法生成时返回 None。"""
        try:
            if kind == "image":
                return self._prepare_image(path)
            if kind == "video":
                return self._prepare_video(path)
        except Exception as exc:  # noqa: BLE001 - 单个文件失败不该中断监控
            log.warning("生成 %s 的内联副本失败：%s", path, exc)
        return None

    def _prepare_image(self, path: str) -> PreparedInline | None:
        ext = os.path.splitext(path)[1].lower()
        size = os.path.getsize(path)
        if ext in ANIMATION_PASSTHROUGH_EXTS and size <= self.video_max_bytes:
            return PreparedInline("animation", path, temporary=False)
        if ext in PHOTO_PASSTHROUGH_EXTS and size <= self.photo_max_bytes:
            return PreparedInline("photo", path, temporary=False)
        if ext == ".gif":
            return None

        target = self._temp_path(path, ".jpg")
        if self._pillow:
            if self._convert_with_pillow(path, target):
                return PreparedInline("photo", target, temporary=True, note="pillow")
        if self._ffmpeg:
            if self._convert_with_ffmpeg(path, target):
                return PreparedInline("photo", target, temporary=True, note="ffmpeg")
        if self._magick:
            if self._convert_with_magick(path, target):
                return PreparedInline("photo", target, temporary=True, note="imagemagick")
        if not any((self._pillow, self._ffmpeg, self._magick)):
            log.info("未安装 Pillow/ffmpeg/ImageMagick，%s 只发原文件", path)
        return None

    def _prepare_video(self, path: str) -> PreparedInline | None:
        ext = os.path.splitext(path)[1].lower()
        size = os.path.getsize(path)
        if ext in VIDEO_PASSTHROUGH_EXTS and size <= self.video_max_bytes:
            return PreparedInline("video", path, temporary=False)
        if not self._ffmpeg:
            log.info("未安装 ffmpeg，%s 只发原文件（容器/体积不适合直接内联播放）", path)
            return None
        target = self._temp_path(path, ".mp4")
        attempts = [
            (MAX_VIDEO_SIDE, 28),
            (854, 32),
            (640, 34),
        ]
        for side, crf in attempts:
            if self._convert_video(path, target, side, crf):
                if os.path.getsize(target) <= self.video_max_bytes:
                    return PreparedInline("video", target, temporary=True, note=f"ffmpeg crf{crf}")
                log.debug("%s 转码后仍超出上限（crf=%d），继续压缩", path, crf)
        log.warning("%s 压缩后仍超过视频上限，只发原文件", path)
        return None

    # ------------------------------------------------------------------ #
    def _temp_path(self, source: str, suffix: str) -> str:
        stem = os.path.splitext(os.path.basename(source))[0][:48]
        handle = tempfile.NamedTemporaryFile(
            prefix=f"{stem}-", suffix=suffix, dir=self.workdir, delete=False
        )
        handle.close()
        return handle.name

    def _convert_with_pillow(self, source: str, target: str) -> bool:
        try:
            with Image.open(source) as image:
                image.load()
                if image.mode in ("RGBA", "LA", "P"):
                    image = image.convert("RGBA")
                    background = Image.new("RGB", image.size, (255, 255, 255))
                    background.paste(image, mask=image.split()[-1])
                    image = background
                elif image.mode != "RGB":
                    image = image.convert("RGB")
                image.thumbnail((MAX_PHOTO_SIDE, MAX_PHOTO_SIDE))
                for quality in (88, 78, 68):
                    image.save(target, "JPEG", quality=quality, optimize=True)
                    if os.path.getsize(target) <= self.photo_max_bytes:
                        return True
            log.debug("%s 转 JPEG 后仍超过图片上限", source)
            return False
        except Exception as exc:  # noqa: BLE001
            log.debug("Pillow 转换 %s 失败：%s", source, exc)
            return False

    def _convert_with_ffmpeg(self, source: str, target: str) -> bool:
        cmd = [
            self._ffmpeg, "-y", "-loglevel", "error", "-i", source,
            "-vf", f"scale='min({MAX_PHOTO_SIDE},iw)':-2", "-frames:v", "1", "-q:v", "3", target,
        ]
        return self._run(cmd, target)

    def _convert_with_magick(self, source: str, target: str) -> bool:
        assert self._magick
        cmd = [
            self._magick, source,
            "-resize", f"{MAX_PHOTO_SIDE}x{MAX_PHOTO_SIDE}>",
            "-quality", "88", target,
        ]
        return self._run(cmd, target)

    def _convert_video(self, source: str, target: str, max_side: int, crf: int) -> bool:
        assert self._ffmpeg
        cmd = [
            self._ffmpeg, "-y", "-loglevel", "error", "-i", source,
            "-vf", f"scale='min({max_side},iw)':-2",
            "-c:v", "libx264", "-preset", "veryfast", "-crf", str(crf),
            "-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", target,
        ]
        return self._run(cmd, target)

    @staticmethod
    def _run(cmd: list[str], target: str) -> bool:
        try:
            result = subprocess.run(
                cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=CONVERT_TIMEOUT
            )
        except (OSError, subprocess.TimeoutExpired) as exc:
            log.debug("命令执行失败 %s：%s", cmd[0], exc)
            return False
        if result.returncode != 0 or not os.path.exists(target):
            log.debug("%s 退出码 %s：%s", cmd[0], result.returncode, result.stderr[:200])
            return False
        return os.path.getsize(target) > 0
