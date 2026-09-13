#!/usr/bin/env python3
"""Extract exactly one regular GoReleaser binary from a bounded archive."""
from __future__ import annotations

import os
import gzip
from pathlib import Path
import stat
import struct
import sys
import tarfile
import tempfile
import zipfile
from contextlib import contextmanager

MAX_ARCHIVE = 512 * 1024 * 1024
MAX_EXPANDED = 512 * 1024 * 1024
MAX_MEMBERS = 32
MAX_METADATA = 64 * 1024
PAX_METADATA_KEYS = {b"mtime", b"atime", b"ctime", b"uname", b"gname", b"uid", b"gid", b"comment"}


def check_pax_metadata(payload: bytes) -> None:
    # Layout/path overrides can make TarFile interpret headers inside a body
    # that physical preflight skipped. Release binaries fit ordinary headers;
    # permit only metadata with no effect on member boundaries or file type.
    offset = 0
    while offset < len(payload):
        space = payload.find(b" ", offset)
        size_text = payload[offset:space] if space >= 0 else b""
        if not size_text.isdigit() or len(size_text) > 8:
            raise ValueError("invalid PAX record length")
        end = offset + int(size_text)
        if end > len(payload) or end <= space + 2 or payload[end - 1:end] != b"\n":
            raise ValueError("invalid PAX record")
        key, separator, _ = payload[space + 1:end - 1].partition(b"=")
        if not separator or key not in PAX_METADATA_KEYS:
            raise ValueError("unsupported PAX metadata key")
        offset = end


def check_zip_inventory(archive: Path) -> None:
    # ZipFile allocates the complete central directory in its constructor.
    # Bound that inventory before handing the input to it. ZIP64 is unnecessary
    # for this contract (one binary, at most 512 MiB) and is rejected.
    with archive.open("rb") as source:
        # Include the locator even when the ZIP comment uses all 65,535 bytes.
        source.seek(max(0, archive.stat().st_size - 65577))
        tail = source.read(65577)
    offset = tail.rfind(b"PK\x05\x06")
    if offset < 0 or len(tail) - offset < 22:
        raise ValueError("missing ZIP directory")
    if offset >= 20 and tail[offset - 20:offset - 16] == b"PK\x06\x07":
        raise ValueError("ZIP64 directory is unsupported")
    _, disk, directory_disk, count_disk, count, size, start, comment = struct.unpack_from("<4s4H2LH", tail, offset)
    if disk or directory_disk or count != count_disk or count > MAX_MEMBERS or size > MAX_METADATA:
        raise ValueError("ZIP inventory exceeds limits or spans disks")
    end = archive.stat().st_size - len(tail) + offset
    if offset + 22 + comment != len(tail) or start + size != end:
        raise ValueError("unsupported ZIP directory layout")


@contextmanager
def bounded_tar(archive: Path):
    # TarFile.getmembers() can inflate an entire hostile gzip stream before a
    # later member-count check. Bound container expansion first, including PAX
    # metadata and padding, then parse the private seekable copy incrementally.
    with archive.open("rb") as raw, tempfile.TemporaryFile() as expanded:
        compressed = raw.read(2) == b"\x1f\x8b"
        raw.seek(0)
        source = gzip.GzipFile(fileobj=raw) if compressed else raw
        try:
            total = 0
            while chunk := source.read(1024 * 1024):
                total += len(chunk)
                if total > MAX_EXPANDED + MAX_METADATA:
                    raise ValueError("expanded tar container exceeds limit")
                expanded.write(chunk)
        finally:
            if compressed:
                source.close()
        expanded.seek(0)
        check_tar_headers(expanded)
        expanded.seek(0)
        with tarfile.open(fileobj=expanded, mode="r:") as tf:
            yield tf


def check_tar_headers(source) -> None:
    # PAX/GNU extension payloads are read and decoded inside TarFile.next(),
    # before normal iteration can inspect them. Check physical headers first.
    source.seek(0, os.SEEK_END)
    length = source.tell()
    source.seek(0)
    count = metadata = 0
    while True:
        header = source.read(512)
        if len(header) != 512:
            raise ValueError("tar end marker is missing")
        if header == b"\0" * 512:
            while chunk := source.read(1024 * 1024):
                if chunk.strip(b"\0"):
                    raise ValueError("data after tar end marker")
            return
        count += 1
        if count > MAX_MEMBERS:
            raise ValueError("tar physical member count exceeds limit")
        member = tarfile.TarInfo.frombuf(header, "utf-8", "surrogateescape")
        if member.size < 0 or member.size > MAX_EXPANDED:
            raise ValueError("tar member exceeds size limit")
        if member.type in (tarfile.XHDTYPE, tarfile.XGLTYPE, tarfile.GNUTYPE_LONGNAME, tarfile.GNUTYPE_LONGLINK):
            metadata += member.size
            if metadata > MAX_METADATA:
                raise ValueError("tar metadata exceeds limit")
            payload = source.read(member.size)
            if b"GNU.sparse" in payload:
                raise ValueError("sparse tar members are unsupported")
            if member.type in (tarfile.XHDTYPE, tarfile.XGLTYPE):
                check_pax_metadata(payload)
            source.seek(-len(payload), os.SEEK_CUR)
        elif member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE):
            raise ValueError("archive contains an unsafe member")
        padded = (member.size + 511) // 512 * 512
        if source.tell() + padded > length:
            raise ValueError("truncated tar member")
        source.seek(padded, os.SEEK_CUR)


def safe_name(name: str, expected: str) -> bool:
    if "\x00" in name or name != expected or Path(name).is_absolute():
        return False
    parts = Path(name).parts
    return not any(part in {"", ".", ".."} for part in parts)


def extract(archive: Path, destination: Path, expected: str) -> None:
    if archive.stat().st_size > MAX_ARCHIVE:
        raise ValueError("archive exceeds size limit")
    destination.mkdir(parents=True, exist_ok=True)
    entries: list[tuple[str, int]] = []
    if archive.suffix == ".zip":
        check_zip_inventory(archive)
        with zipfile.ZipFile(archive) as zf:
            infos = zf.infolist()
            if len(infos) > MAX_MEMBERS:
                raise ValueError("archive member count exceeds limit")
            for info in infos:
                mode = (info.external_attr >> 16) & 0o170000
                if not safe_name(info.filename, expected) or mode not in (0, stat.S_IFREG):
                    raise ValueError("archive contains an unsafe member")
                if info.file_size > MAX_EXPANDED:
                    raise ValueError("archive member exceeds size limit")
                entries.append((info.filename, info.file_size))
            if len(entries) != 1:
                raise ValueError("archive must contain exactly one binary")
            with zf.open(infos[0]) as source, (destination / expected).open("xb") as target:
                total = 0
                while chunk := source.read(1024 * 1024):
                    total += len(chunk)
                    if total > MAX_EXPANDED:
                        raise ValueError("expanded archive exceeds limit")
                    target.write(chunk)
    else:
        with bounded_tar(archive) as tf:
            members = []
            for member in tf:
                if len(members) >= MAX_MEMBERS:
                    raise ValueError("archive member count exceeds limit")
                members.append(member)
                if not safe_name(member.name, expected) or not member.isreg():
                    raise ValueError("archive contains an unsafe member")
                if member.size > MAX_EXPANDED:
                    raise ValueError("archive member exceeds size limit")
                entries.append((member.name, member.size))
            if len(entries) != 1:
                raise ValueError("archive must contain exactly one binary")
            source = tf.extractfile(members[0])
            if source is None:
                raise ValueError("archive member cannot be read")
            with source, (destination / expected).open("xb") as target:
                total = 0
                while chunk := source.read(1024 * 1024):
                    total += len(chunk)
                    if total > MAX_EXPANDED:
                        raise ValueError("expanded archive exceeds limit")
                    target.write(chunk)
    os.chmod(destination / expected, 0o700)


if __name__ == "__main__":
    if len(sys.argv) != 4:
        raise SystemExit("usage: safe_extract.py ARCHIVE DESTINATION BINARY")
    try:
        extract(Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3])
    except (OSError, EOFError, ValueError, tarfile.TarError, zipfile.BadZipFile) as exc:
        print(f"safe-extract: {exc}", file=sys.stderr)
        raise SystemExit(1)
