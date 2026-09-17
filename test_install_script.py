"""Linux regression tests for repository and standalone /tmp installer invocation."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


@unittest.skipUnless(shutil.which("bash") and os.name != "nt", "requires Linux bash")
class InstallerSourceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="monitoring-install-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.script = Path(__file__).parent / "deploy/install.sh"

    def fixture(self, path):
        path.mkdir(parents=True)
        for name in ("go.mod", "main.go", "database.go", ".env.example"):
            (path / name).write_text("fixture\n")
        return path

    def run_installer(self, script, destination, **extra):
        return subprocess.run(["bash", str(script), str(destination)], text=True,
            capture_output=True, env=dict(os.environ, CHECK_SOURCE_ONLY="1", **extra))

    def test_checkout_uses_own_source(self):
        source = self.fixture(self.root / "source")
        (source / "deploy").mkdir()
        script = source / "deploy/install.sh"
        shutil.copyfile(self.script, script)
        result = self.run_installer(script, self.root / "destination")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(str(source), result.stdout)
        self.assertFalse((self.root / "destination").exists())

    def test_tmp_script_uses_installed_source_and_preserves_env(self):
        source = self.fixture(self.root / "app")
        (source / ".env").write_text("MYSQL_PASSWORD=preserve-this\n")
        script = self.root / "install.sh"
        shutil.copyfile(self.script, script)
        result = self.run_installer(script, source)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(str(source), result.stdout)
        self.assertEqual((source / ".env").read_text(), "MYSQL_PASSWORD=preserve-this\n")

    def test_tmp_script_clones_then_fast_forwards(self):
        source = self.fixture(self.root / "origin")
        def git(*args):
            return subprocess.run(["git", "-C", str(source), *args], check=True, capture_output=True)
        git("init", "-b", "main")
        git("config", "user.email", "test@example.invalid")
        git("config", "user.name", "Installer Test")
        git("add", ".")
        git("commit", "-m", "initial")
        script = self.root / "install.sh"
        shutil.copyfile(self.script, script)
        target = self.root / "destination"
        first = self.run_installer(script, target, REPO_URL=str(source))
        self.assertEqual(first.returncode, 0, first.stderr)
        (source / "main.go").write_text("updated\n")
        git("add", ".")
        git("commit", "-m", "update")
        second = self.run_installer(script, target, REPO_URL=str(source))
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual((target / "main.go").read_text(), "updated\n")


if __name__ == "__main__":
    unittest.main()
