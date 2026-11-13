import json
import unittest

from codex_checks import outcomes


class CheckResults(unittest.TestCase):
    def events(self, *events):
        return "\n".join(json.dumps({"Package": "example/convo", **event}) for event in events)

    def test_success_needs_expected_tests(self):
        log = self.events({"Action": "pass", "Test": "TestCodex"}, {"Action": "pass"})
        self.assertTrue(outcomes(log, 0, ["TestCodex"])["passed"])
        self.assertFalse(outcomes(log, 0, ["TestCodex", "TestMissing"])["passed"])

    def test_empty_and_package_only_success_are_failures(self):
        for log in ("", "not JSON", self.events({"Action": "pass"})):
            self.assertFalse(outcomes(log, 0, [])["passed"])

    def test_skips_fail_even_when_go_succeeds(self):
        log = self.events({"Action": "skip", "Test": "TestCodex"}, {"Action": "pass"})
        self.assertFalse(outcomes(log, 0, ["TestCodex"])["passed"])

    def test_subtest_failure_and_abnormal_exit_are_failures(self):
        passed = self.events({"Action": "pass", "Test": "TestCodex"})
        self.assertFalse(outcomes(passed, 1, ["TestCodex"])["passed"])
        failed = passed + "\n" + self.events({"Action": "fail", "Test": "TestCodex/approval"})
        self.assertFalse(outcomes(failed, 0, ["TestCodex"])["passed"])


if __name__ == "__main__":
    unittest.main()
