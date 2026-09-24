"""文件分类与"可点开副本"生成。转换工具用桩程序替代，验证调用链路与降级行为。"""

from __future__ import annotations

import os
import shutil
import stat
import tempfile
import unittest

from filebot.media import MediaPreparer, classify

STUB_TOOL = """#!/bin/sh
in=""
out=""
first="$1"
while [ $# -gt 0 ]; do
  if [ "$1" = "-i" ]; then in="$2"; shift 2; continue; fi
  out="$1"; shift
done
if [ -z "$in" ]; then in="$first"; fi
cp "$in" "$out"
"""


def make_stub_tool(directory: str, name: str = "ffmpeg") -> str:
    path = os.path.join(directory, name)
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(STUB_TOOL)
    os.chmod(path, os.stat(path).st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return path


class MediaTestBase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.stub_dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.stub_dir, True)
        self.ffmpeg = make_stub_tool(self.stub_dir)

    def write(self, name: str, size: int = 2048) -> str:
        path = os.path.join(self.tmp.name, name)
        with open(path, "wb") as handle:
            handle.write(b"A" * size)
        return path

    def preparer(self, **kwargs) -> MediaPreparer:
        options = {
            "photo_max_bytes": 10 * 1024 * 1024,
            "video_max_bytes": 50 * 1024 * 1024,
            "ffmpeg": None,
            "magick": None,
            "use_pillow": False,
        }
        options.update(kwargs)
        return MediaPreparer(**options)


class TestClassify(unittest.TestCase):
    def test_known_extensions(self):
        self.assertEqual(classify("/a/b.JPG"), "image")
        self.assertEqual(classify("/a/b.mkv"), "video")
        self.assertEqual(classify("/a/b.mp4"), "video")
        self.assertIsNone(classify("/a/b.txt"))
        self.assertIsNone(classify("/a/b"))


class TestPassthrough(MediaTestBase):
    def test_small_jpeg_is_sent_as_is(self):
        path = self.write("photo.jpg")
        prepared = self.preparer().prepare(path, "image")
        self.assertEqual(prepared.kind, "photo")
        self.assertFalse(prepared.temporary)
        self.assertEqual(prepared.path, path)

    def test_png_passthrough(self):
        prepared = self.preparer().prepare(self.write("shot.png", 1000), "image")
        self.assertEqual(prepared.kind, "photo")
        self.assertFalse(prepared.temporary)

    def test_gif_becomes_animation(self):
        prepared = self.preparer().prepare(self.write("anim.gif", 1000), "image")
        self.assertEqual(prepared.kind, "animation")
        self.assertFalse(prepared.temporary)

    def test_mp4_within_limit_is_video(self):
        path = self.write("clip.mp4", 5000)
        prepared = self.preparer().prepare(path, "video")
        self.assertEqual(prepared.kind, "video")
        self.assertFalse(prepared.temporary)

    def test_large_jpeg_needs_conversion(self):
        # 图片超过 10MB 上限，必须转换（此时没有工具 → 放弃）
        path = self.write("big.jpg", 5000)
        prepared = self.preparer(photo_max_bytes=1000).prepare(path, "image")
        self.assertIsNone(prepared)

    def test_bmp_without_tools_is_skipped(self):
        self.assertIsNone(self.preparer().prepare(self.write("a.bmp"), "image"))

    def test_big_video_without_ffmpeg_is_skipped(self):
        self.assertIsNone(self.preparer().prepare(self.write("a.mkv"), "video"))


class TestConversion(MediaTestBase):
    def test_bmp_converted_to_jpeg_by_ffmpeg(self):
        path = self.write("scan.bmp")
        preparer = self.preparer(ffmpeg=self.ffmpeg)
        prepared = preparer.prepare(path, "image")
        self.assertEqual(prepared.kind, "photo")
        self.assertTrue(prepared.temporary)
        self.assertTrue(prepared.path.endswith(".jpg"))
        self.assertTrue(os.path.exists(prepared.path))
        with open(prepared.path, "rb") as produced, open(path, "rb") as source:
            self.assertEqual(produced.read(), source.read())
        self.assertEqual(prepared.note, "ffmpeg")
        preparer.close()

    def test_mkv_transcoded_by_ffmpeg(self):
        path = self.write("movie.mkv", 3000)
        preparer = self.preparer(ffmpeg=self.ffmpeg)
        prepared = preparer.prepare(path, "video")
        self.assertEqual(prepared.kind, "video")
        self.assertTrue(prepared.temporary)
        self.assertTrue(prepared.path.endswith(".mp4"))
        preparer.close()

    def test_video_gives_up_when_still_too_large(self):
        path = self.write("huge.mkv", 3000)
        preparer = self.preparer(ffmpeg=self.ffmpeg, video_max_bytes=10)
        self.assertIsNone(preparer.prepare(path, "video"))
        preparer.close()

    def test_magick_is_used_when_ffmpeg_missing(self):
        magick = make_stub_tool(self.stub_dir, "magick")
        preparer = self.preparer(magick=magick)
        prepared = preparer.prepare(self.write("a.bmp"), "image")
        self.assertEqual(prepared.note, "imagemagick")
        preparer.close()

    def test_workdir_is_cleaned_up(self):
        preparer = self.preparer(ffmpeg=self.ffmpeg)
        workdir = preparer.workdir
        self.assertTrue(os.path.isdir(workdir))
        preparer.close()
        self.assertFalse(os.path.exists(workdir))

    def test_capabilities_reports_tools(self):
        self.assertIn("ffmpeg", self.preparer(ffmpeg=self.ffmpeg).capabilities())
        self.assertIn("无", self.preparer().capabilities())


if __name__ == "__main__":
    unittest.main()
