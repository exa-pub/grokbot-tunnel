"""Unpacks the desktop client's app/ out of its AppImage without running it.

    unpack.py <AppImage> <sha256> <out>

Writes <out>/app/package.json and <out>/app/dist (without the renderer, which
no test reads).
"""

import hashlib
import json
import os
import struct
import subprocess
import sys
import tempfile


def squashfs_offset(path):
    """The AppImage is an ELF runtime followed by a squashfs image."""
    with open(path, "rb") as f:
        header = f.read(64)
        shoff = struct.unpack_from("<Q", header, 0x28)[0]
        shentsize, shnum = struct.unpack_from("<HH", header, 0x3A)
        offset = shoff + shentsize * shnum
        f.seek(offset)
        if f.read(4) != b"hsqs":
            sys.exit(f"{path}: no squashfs image after the ELF runtime")
    return offset


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def extract_asar(asar, out, skip):
    """Extracts an asar archive: a pickled JSON index, then the files."""
    with open(asar, "rb") as f:
        _, header_size, _, json_size = struct.unpack("<4I", f.read(16))
        index = json.loads(f.read(json_size))
        base = 8 + header_size

        def walk(node, rel):
            for name, entry in node["files"].items():
                path = os.path.join(rel, name)
                if any(path == s or path.startswith(s + "/") for s in skip):
                    continue
                target = os.path.join(out, path)
                if "files" in entry:
                    os.makedirs(target, exist_ok=True)
                    walk(entry, path)
                elif "link" in entry:
                    os.symlink(entry["link"], target)
                elif entry.get("unpacked"):
                    unpacked = os.path.join(asar + ".unpacked", path)
                    with open(unpacked, "rb") as src, open(target, "wb") as dst:
                        dst.write(src.read())
                else:
                    f.seek(base + int(entry["offset"]))
                    with open(target, "wb") as dst:
                        dst.write(f.read(entry["size"]))
                    if entry.get("executable"):
                        os.chmod(target, 0o755)

        os.makedirs(out, exist_ok=True)
        walk(index, "")


def main():
    appimage, want, out = sys.argv[1:]
    got = sha256(appimage)
    if got != want:
        sys.exit(f"{appimage}: sha256 {got}, want {want}")
    with tempfile.TemporaryDirectory() as tmp:
        subprocess.run(
            ["unsquashfs", "-q", "-n", "-o", str(squashfs_offset(appimage)), "-d", os.path.join(tmp, "root"),
             appimage, "resources/app.asar", "resources/app.asar.unpacked"],
            check=True,
        )
        extract_asar(os.path.join(tmp, "root", "resources", "app.asar"), os.path.join(out, "app"), skip=["dist/renderer"])
    for name in ("package.json", "dist/local-exec-daemon/main.cjs", "dist/electron-main/main-core.cjs"):
        if not os.path.isfile(os.path.join(out, "app", name)):
            sys.exit(f"{appimage}: {name} is missing from the bundle")


if __name__ == "__main__":
    main()
