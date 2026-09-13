import importlib.util
import gzip
import struct
import io
import tarfile
import tempfile
from pathlib import Path
import unittest
import zipfile
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("safe_extract", Path(__file__).parents[2] / "scripts/safe_extract.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class SafeExtractTests(unittest.TestCase):
    def make_tar(self, root, names):
        archive = root / "archive.tar.gz"
        with tarfile.open(archive, "w:gz") as output:
            for name, data in names:
                info = tarfile.TarInfo(name)
                info.size = len(data)
                output.addfile(info, io.BytesIO(data))
        return archive

    def test_rejects_traversal_before_writing(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = self.make_tar(root, [("codex-dispatch", b"ok"), ("../escape", b"bad")])
            with self.assertRaises(ValueError):
                MODULE.extract(archive, root / "out", "codex-dispatch")
            self.assertFalse((root / "escape").exists())

    def test_accepts_exact_regular_member(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = self.make_tar(root, [("codex-dispatch", b"ok")])
            MODULE.extract(archive, root / "out", "codex-dispatch")
            self.assertEqual((root / "out/codex-dispatch").read_bytes(), b"ok")

    def test_rejects_symlink_member(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = root / "archive.tar.gz"
            with tarfile.open(archive, "w:gz") as output:
                info = tarfile.TarInfo("codex-dispatch")
                info.type = tarfile.SYMTYPE
                info.linkname = "/etc/passwd"
                output.addfile(info)
            with self.assertRaises(ValueError):
                MODULE.extract(archive, root / "out", "codex-dispatch")

    def test_compressed_container_padding_is_bounded_before_extracting(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = self.make_tar(root, [("codex-dispatch", b"ok")])
            raw = gzip.decompress(archive.read_bytes()) + b"\0" * 100000
            archive.write_bytes(gzip.compress(raw))
            with patch.object(MODULE, "MAX_EXPANDED", 1024), self.assertRaisesRegex(ValueError, "container exceeds"):
                MODULE.extract(archive, root / "out", "codex-dispatch")
            self.assertFalse((root / "out/codex-dispatch").exists())

    def test_zip_inventory_is_rejected_before_zipfile_allocates_it(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = root / "archive.zip"
            with zipfile.ZipFile(archive, "w") as output:
                for i in range(MODULE.MAX_MEMBERS + 1):
                    output.writestr(str(i), b"")
            with patch.object(MODULE.zipfile, "ZipFile", side_effect=AssertionError("parsed unbounded inventory")):
                with self.assertRaisesRegex(ValueError, "inventory exceeds"):
                    MODULE.extract(archive, root / "out", "codex-dispatch")

    def test_zip_regular_binary_and_symlink_control(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = root / "archive.zip"
            with zipfile.ZipFile(archive, "w") as output:
                output.writestr("codex-dispatch.exe", b"binary")
            MODULE.extract(archive, root / "valid", "codex-dispatch.exe")
            self.assertEqual((root / "valid/codex-dispatch.exe").read_bytes(), b"binary")
            with zipfile.ZipFile(archive, "w") as output:
                entry = zipfile.ZipInfo("codex-dispatch.exe")
                entry.create_system = 3
                entry.external_attr = 0o120777 << 16
                output.writestr(entry, "/outside")
            with self.assertRaisesRegex(ValueError, "unsafe member"):
                MODULE.extract(archive, root / "invalid", "codex-dispatch.exe")

    def test_zip64_cannot_override_bounded_classic_inventory(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = root / "archive.zip"
            with zipfile.ZipFile(archive, "w") as output:
                for i in range(MODULE.MAX_MEMBERS + 1):
                    output.writestr(str(i), b"")
            raw = archive.read_bytes()
            offset = raw.rfind(b"PK\x05\x06")
            _, _, _, count_disk, count, size, start, _ = struct.unpack_from("<4s4H2LH", raw, offset)
            record = struct.pack("<4sQ2H2L4Q", b"PK\x06\x06", 44, 45, 45, 0, 0, count_disk, count, size, start)
            locator = struct.pack("<4sLQL", b"PK\x06\x07", 0, offset, 1)
            # The classic record advertises an empty small directory. ZipFile
            # prefers the larger ZIP64 inventory unless preflight rejects it.
            for comment in (b"", b"x" * 65535):
                with self.subTest(comment_length=len(comment)):
                    classic = struct.pack("<4s4H2LH", b"PK\x05\x06", 0, 0, 1, 1, 0, offset + len(record) + len(locator), len(comment))
                    archive.write_bytes(raw[:offset] + record + locator + classic + comment)
                    with patch.object(MODULE.zipfile, "ZipFile", side_effect=AssertionError("parsed ZIP64 override")):
                        with self.assertRaisesRegex(ValueError, "ZIP64"):
                            MODULE.extract(archive, root / "out", "codex-dispatch")

    def test_tar_metadata_is_bounded_before_pax_decoding(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = root / "archive.tar.gz"
            with tarfile.open(archive, "w:gz", format=tarfile.PAX_FORMAT) as output:
                info = tarfile.TarInfo("codex-dispatch")
                info.size = 2
                info.pax_headers = {"comment": "x" * (MODULE.MAX_METADATA + 1)}
                output.addfile(info, io.BytesIO(b"ok"))
            with patch.object(MODULE.tarfile, "open", side_effect=AssertionError("decoded oversized PAX")):
                with self.assertRaisesRegex(ValueError, "metadata exceeds"):
                    MODULE.extract(archive, root / "out", "codex-dispatch")
            self.assertFalse((root / "out/codex-dispatch").exists())

    def test_regular_pax_metadata_and_sparse_override(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = root / "archive.tar.gz"
            for sparse in (False, True):
                with tarfile.open(archive, "w:gz", format=tarfile.PAX_FORMAT) as output:
                    info = tarfile.TarInfo("codex-dispatch")
                    info.size = 2
                    info.pax_headers = {"mtime": "1.25"}
                    if sparse:
                        info.pax_headers.update({"GNU.sparse.major": "1", "GNU.sparse.minor": "0"})
                    output.addfile(info, io.BytesIO(b"ok"))
                if sparse:
                    with self.assertRaisesRegex(ValueError, "sparse tar"):
                        MODULE.extract(archive, root / "invalid", "codex-dispatch")
                else:
                    MODULE.extract(archive, root / "valid", "codex-dispatch")
                    self.assertEqual((root / "valid/codex-dispatch").read_bytes(), b"ok")

    def test_pax_size_override_cannot_reinterpret_preflighted_body(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archive = root / "archive.tar.gz"
            with tarfile.open(archive, "w:gz", format=tarfile.PAX_FORMAT) as output:
                info = tarfile.TarInfo("codex-dispatch")
                info.size = 1024
                info.pax_headers = {"size": "0"}
                hidden = tarfile.TarInfo("hidden")
                hidden.type = tarfile.XHDTYPE
                hidden.size = MODULE.MAX_METADATA + 1
                output.addfile(info, io.BytesIO(hidden.tobuf() + b"x" * 512))
            with patch.object(MODULE.tarfile, "open", side_effect=AssertionError("parsed layout override")):
                with self.assertRaisesRegex(ValueError, "unsupported PAX"):
                    MODULE.extract(archive, root / "out", "codex-dispatch")


if __name__ == "__main__":
    unittest.main()
