import hashlib
import io
import json
import pathlib
import tarfile
import tempfile
import unittest

from validate_oci import validate


class OCITest(unittest.TestCase):
    def fixture(self):
        files = {}

        def blob(data, media):
            digest = hashlib.sha256(data).hexdigest()
            files["blobs/sha256/" + digest] = data
            return {"digest": "sha256:" + digest, "size": len(data), "mediaType": media}

        config = blob(b'{"os":"linux","architecture":"amd64"}', "application/vnd.oci.image.config.v1+json")
        layer = blob(b"opaque layer bytes NEVER unpacked or run", "application/vnd.oci.image.layer.v1.tar")
        manifest = blob(json.dumps({"schemaVersion": 2, "config": config, "layers": [layer]}).encode(), "application/vnd.oci.image.manifest.v1+json")
        files["oci-layout"] = b'{"imageLayoutVersion":"1.0.0"}'
        files["index.json"] = json.dumps({"schemaVersion": 2, "manifests": [manifest]}).encode()
        return files, manifest["digest"]

    def run_archive(self, files, extra=None, limit=1024*1024):
        with tempfile.TemporaryDirectory() as tmp:
            archive = pathlib.Path(tmp) / "test.tar"
            with tarfile.open(archive, "w") as tar:
                for name, data in files.items():
                    info = tarfile.TarInfo(name)
                    info.size = len(data)
                    tar.addfile(info, io.BytesIO(data))
                if extra:
                    tar.addfile(extra)
            return validate(archive, pathlib.Path(tmp) / "validated", limit)

    def test_valid(self):
        files, digest = self.fixture()
        self.assertEqual(self.run_archive(files), digest)

    def test_paths_and_links(self):
        for name, kind in [("../escape", tarfile.REGTYPE), ("/absolute", tarfile.REGTYPE),
                           ("blobs/sha256/nope", tarfile.REGTYPE), ("index.json", tarfile.SYMTYPE),
                           ("oci-layout", tarfile.LNKTYPE), ("unexpected", tarfile.DIRTYPE)]:
            with self.subTest(name=name, kind=kind):
                files, _ = self.fixture()
                entry = tarfile.TarInfo(name)
                entry.type, entry.linkname = kind, "/etc/passwd"
                with self.assertRaises(ValueError):
                    self.run_archive(files, entry)

    def test_duplicate(self):
        files, _ = self.fixture()
        with self.assertRaises(ValueError):
            self.run_archive(files, tarfile.TarInfo("index.json"))

    def test_bad_hash(self):
        files, _ = self.fixture()
        files[next(iter(files))] += b"changed"
        with self.assertRaises(ValueError):
            self.run_archive(files)

    def test_external_and_ambiguous_descriptors(self):
        for change in [lambda index: index["manifests"][0].update(urls=["https://evil.invalid/layer"]),
                       lambda index: index["manifests"].append(index["manifests"][0]),
                       lambda index: index["manifests"][0].update(size=0),
                       lambda index: index["manifests"][0].update(mediaType="foreign")]:
            files, _ = self.fixture()
            index = json.loads(files["index.json"])
            change(index)
            files["index.json"] = json.dumps(index).encode()
            with self.assertRaises(ValueError):
                self.run_archive(files)

    def test_bound(self):
        files, _ = self.fixture()
        with self.assertRaises(ValueError):
            self.run_archive(files, limit=10)


if __name__ == "__main__":
    unittest.main()
