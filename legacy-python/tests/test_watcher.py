"""目录监控：新增/变化检测、去抖、过滤规则（用注入的 now 模拟时间，不真的 sleep）。"""

from __future__ import annotations

import os
import tempfile
import unittest

from filebot.watcher import DirectoryWatcher, scan


class WatcherTestBase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = self.tmp.name

    def make_watcher(self, **kwargs) -> DirectoryWatcher:
        options = {
            "poll_interval": 0.01,
            "settle_seconds": 3.0,
            "min_size": 0,
        }
        options.update(kwargs)
        return DirectoryWatcher([self.root], **options)

    def write(self, relative: str, size: int = 2048) -> str:
        path = os.path.join(self.root, relative)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as handle:
            handle.write(b"x" * size)
        return path


class TestDetection(WatcherTestBase):
    def test_new_file_in_subdirectory_is_emitted_after_settle(self):
        watcher = self.make_watcher()
        watcher.poll(now=0)
        path = self.write("sub/dir/clip.mp4")

        self.assertEqual(watcher.poll(now=1), [])
        ready = watcher.poll(now=10)
        self.assertEqual([item.path for item in ready], [path])
        self.assertEqual(ready[0].size, 2048)

    def test_file_still_being_copied_is_not_emitted(self):
        watcher = self.make_watcher()
        watcher.poll(now=0)
        path = self.write("growing.mp4", 100)

        self.assertEqual(watcher.poll(now=1), [])
        with open(path, "ab") as handle:  # 模拟拷贝继续
            handle.write(b"y" * 500)
        self.assertEqual(watcher.poll(now=2), [])
        with open(path, "ab") as handle:
            handle.write(b"y" * 500)
        self.assertEqual(watcher.poll(now=3), [])

        ready = watcher.poll(now=10)
        self.assertEqual(len(ready), 1)
        self.assertEqual(ready[0].size, 1100)

    def test_modified_file_is_emitted_again(self):
        watcher = self.make_watcher()
        watcher.poll(now=0)
        path = self.write("a.jpg")
        watcher.poll(now=1)
        self.assertEqual(len(watcher.poll(now=10)), 1)

        with open(path, "ab") as handle:
            handle.write(b"z" * 10)
        self.assertEqual(watcher.poll(now=11), [])
        self.assertEqual(len(watcher.poll(now=20)), 1)

    def test_deleted_file_is_dropped(self):
        watcher = self.make_watcher()
        watcher.poll(now=0)
        path = self.write("temp.mp4")
        watcher.poll(now=1)
        os.unlink(path)
        self.assertEqual(watcher.poll(now=10), [])

    def test_scan_existing_emits_after_settle(self):
        path = self.write("old.jpg")
        watcher = self.make_watcher(scan_existing=True)
        self.assertEqual(watcher.poll(now=0), [])
        self.assertEqual([item.path for item in watcher.poll(now=10)], [path])

    def test_scan_existing_with_zero_settle_emits_immediately(self):
        path = self.write("old.jpg")
        watcher = self.make_watcher(scan_existing=True, settle_seconds=0.0)
        self.assertEqual([item.path for item in watcher.poll(now=0)], [path])

    def test_default_does_not_emit_pre_existing_files(self):
        self.write("old.jpg")
        watcher = self.make_watcher()
        self.assertEqual(watcher.poll(now=0), [])
        self.assertEqual(watcher.poll(now=100), [])


class TestFilters(WatcherTestBase):
    def test_hidden_and_temp_files_are_ignored(self):
        watcher = self.make_watcher()
        watcher.poll(now=0)
        self.write(".hidden.jpg")
        self.write("movie.mp4.part")
        self.write("movie.mp4.crdownload")
        self.write("draft.jpg~")
        os.makedirs(os.path.join(self.root, ".cache"), exist_ok=True)
        self.write(".cache/x.jpg")
        self.assertEqual(watcher.poll(now=10), [])

    def test_min_size_filter(self):
        watcher = self.make_watcher(min_size=1024)
        watcher.poll(now=0)
        self.write("tiny.jpg", 100)
        big = self.write("ok.jpg", 5000)
        watcher.poll(now=1)
        ready = watcher.poll(now=10)
        self.assertEqual([item.path for item in ready], [big])

    def test_exclude_patterns(self):
        watcher = self.make_watcher(excludes=["*.tmp", "*thumbnail*"])
        watcher.poll(now=0)
        self.write("a.tmp", 2000)
        self.write("thumbnail_1.jpg", 2000)
        keep = self.write("real.jpg", 2000)
        watcher.poll(now=1)
        ready = watcher.poll(now=10)
        self.assertEqual([item.path for item in ready], [keep])


class TestScan(unittest.TestCase):
    def test_scan_reports_size_and_mtime(self):
        with tempfile.TemporaryDirectory() as tmp:
            nested = os.path.join(tmp, "a", "b")
            os.makedirs(nested)
            path = os.path.join(nested, "f.mp4")
            with open(path, "wb") as handle:
                handle.write(b"q" * 42)
            found = scan([tmp])
            self.assertIn(path, found)
            self.assertEqual(found[path][1], 42)

    def test_scan_missing_directory_is_empty(self):
        self.assertEqual(scan(["/definitely/not/here"]), {})


if __name__ == "__main__":
    unittest.main()
