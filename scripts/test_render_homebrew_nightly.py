#!/usr/bin/env python3
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPT = REPO_ROOT / "scripts" / "render-homebrew-nightly.py"
VERSION = "v0.7.0-nightly.20260509.g1a2b3c4"


def checksum_line(index, filename):
    return f"{index:064x}  {filename}\n"


class RenderHomebrewNightlyTests(unittest.TestCase):
    def run_renderer(self, filenames):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            checksums = tmp_path / "checksums.txt"
            output = tmp_path / "Formula" / "lynxdb-nightly.rb"
            checksums.write_text("".join(checksum_line(i, name) for i, name in enumerate(filenames, start=1)))

            result = subprocess.run(
                [
                    sys.executable,
                    str(SCRIPT),
                    "--version",
                    VERSION,
                    "--checksums",
                    str(checksums),
                    "--output",
                    str(output),
                ],
                text=True,
                capture_output=True,
                check=False,
            )
            formula = output.read_text() if output.exists() else ""
            return result, formula

    def test_renders_nightly_formula(self):
        result, formula = self.run_renderer(
            [
                "lynxdb-v0.7.0-nightly.20260509.g1a2b3c4-darwin-amd64.tar.gz",
                "lynxdb-v0.7.0-nightly.20260509.g1a2b3c4-darwin-arm64.tar.gz",
                "lynxdb-v0.7.0-nightly.20260509.g1a2b3c4-linux-amd64.tar.gz",
                "lynxdb-v0.7.0-nightly.20260509.g1a2b3c4-linux-arm64.tar.gz",
            ]
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("class LynxdbNightly < Formula", formula)
        self.assertIn('conflicts_with "lynxdb"', formula)
        self.assertIn('bin.install "lynxdb"', formula)
        self.assertIn("nightly prerelease build", formula)
        self.assertIn("https://dl.lynxdb.org/v0.7.0-nightly.20260509.g1a2b3c4/", formula)
        self.assertNotIn("manifest.json", formula)

    def test_missing_platform_fails(self):
        result, formula = self.run_renderer(
            [
                "lynxdb-v0.7.0-nightly.20260509.g1a2b3c4-darwin-amd64.tar.gz",
                "lynxdb-v0.7.0-nightly.20260509.g1a2b3c4-darwin-arm64.tar.gz",
                "lynxdb-v0.7.0-nightly.20260509.g1a2b3c4-linux-amd64.tar.gz",
            ]
        )

        self.assertEqual(result.returncode, 1)
        self.assertEqual(formula, "")
        self.assertIn("missing required artifacts: linux-arm64", result.stderr)


if __name__ == "__main__":
    unittest.main()
