"""Validate an untrusted OCI archive into a new directory; never unpack layers."""

import hashlib
import json
import pathlib
import re
import sys
import tarfile

MIB = 1024 * 1024
BLOB = re.compile(r"blobs/sha256/[0-9a-f]{64}\Z")
IMAGE = {"application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"}
CONFIG = {"application/vnd.oci.image.config.v1+json", "application/vnd.docker.container.image.v1+json"}
LAYER = {"application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.oci.image.layer.v1.tar", "application/vnd.docker.image.rootfs.diff.tar.gzip"}


def require(ok, message):
    if not ok:
        raise ValueError(message)


def validate(archive, output, max_bytes):
    output = pathlib.Path(output)
    require(pathlib.Path(archive).stat().st_size <= max_bytes, "archive too large")
    output.mkdir(mode=0o700)  # Must not exist; never write into a checkout or shared directory.
    sizes = {}
    with tarfile.open(archive, mode="r:") as tar:
        total = 0
        for i, entry in enumerate(tar):
            require(i < 512, "too many entries")
            name = entry.name
            if entry.isdir() and name in {"blobs", "blobs/sha256"}:
                continue
            require(entry.isreg() and not entry.sparse, "regular files only")
            require(name in {"oci-layout", "index.json"} or BLOB.fullmatch(name), "unsafe path")
            require(name not in sizes, "duplicate path")
            total += entry.size
            require(0 <= entry.size <= max_bytes and total <= max_bytes, "unpacked size limit")
            if not BLOB.fullmatch(name):
                require(entry.size <= 2 * MIB, "metadata too large")
            dest = output / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            digest = hashlib.sha256()
            remaining = entry.size
            with tar.extractfile(entry) as source, dest.open("xb") as target:
                while remaining:
                    chunk = source.read(min(MIB, remaining))
                    require(chunk, "truncated entry")
                    target.write(chunk)
                    digest.update(chunk)
                    remaining -= len(chunk)
            if BLOB.fullmatch(name):
                require(digest.hexdigest() == name.rsplit("/", 1)[1], "blob digest mismatch")
            sizes[name] = entry.size

    def read_json(name):
        require(name in sizes and sizes[name] <= 2 * MIB, "missing or oversized JSON")
        value = json.loads((output / name).read_text(encoding="utf-8"))
        require(isinstance(value, dict), "JSON object required")
        return value

    referenced = set()

    def descriptor(desc, types):
        require(isinstance(desc, dict) and not desc.get("urls") and not desc.get("data"), "external/inline content refused")
        require(desc.get("mediaType") in types, "unsupported media type")
        digest = desc.get("digest", "")
        require(re.fullmatch(r"sha256:[0-9a-f]{64}", digest), "invalid digest")
        name = "blobs/sha256/" + digest[7:]
        require(name in sizes and sizes[name] == desc.get("size"), "descriptor size mismatch")
        referenced.add(name)
        return name

    require(read_json("oci-layout") == {"imageLayoutVersion": "1.0.0"}, "OCI layout version")
    index = read_json("index.json")
    require(index.get("schemaVersion") == 2 and len(index.get("manifests", [])) == 1, "single image required")
    manifest_name = descriptor(index["manifests"][0], IMAGE)
    manifest = read_json(manifest_name)
    require(manifest.get("schemaVersion") == 2, "manifest schema")
    config_name = descriptor(manifest.get("config"), CONFIG)
    config = read_json(config_name)
    require(config.get("os") == "linux" and config.get("architecture") == "amd64", "linux/amd64 required")
    layers = manifest.get("layers")
    require(isinstance(layers, list) and 0 < len(layers) <= 64, "layer bounds")
    for layer in layers:
        descriptor(layer, LAYER)
    require(set(sizes) == referenced | {"index.json", "oci-layout"}, "unreferenced content")
    return "sha256:" + manifest_name.rsplit("/", 1)[1]


if __name__ == "__main__":
    archive, output, kind = sys.argv[1:]
    require(kind in {"runtime", "agent"}, "unknown image kind")
    print(validate(archive, output, (128 if kind == "runtime" else 2048) * MIB))
