#!/usr/bin/env python3

# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import importlib.util
import io
import json
import pathlib
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock


SCRIPT = pathlib.Path(__file__).with_name("update-containerd-service-template.py")


def load_updater():
    spec = importlib.util.spec_from_file_location(
        "update_containerd_service_template", SCRIPT
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


class UpdateContainerdServiceTemplateTests(unittest.TestCase):
    def setUp(self):
        self.updater = load_updater()

    def configure_fixture(
        self,
        root,
        generic_version,
        ppc64le_version,
        template_content,
        template_versions,
    ):
        containerd_config = root / "packer" / "config" / "containerd.json"
        ppc64le_config = root / "packer" / "config" / "ppc64le" / "containerd.json"
        template_path = (
            root
            / "ansible"
            / "roles"
            / "containerd"
            / "templates"
            / "etc"
            / "systemd"
            / "system"
            / "containerd.service"
        )
        defaults_path = (
            root
            / "ansible"
            / "roles"
            / "containerd"
            / "defaults"
            / "main.yml"
        )

        containerd_config.parent.mkdir(parents=True)
        ppc64le_config.parent.mkdir(parents=True)
        template_path.parent.mkdir(parents=True)
        defaults_path.parent.mkdir(parents=True)

        containerd_config.write_text(
            json.dumps({"containerd_version": generic_version}), encoding="utf-8"
        )
        ppc64le_config.write_text(
            json.dumps({"containerd_version": ppc64le_version}), encoding="utf-8"
        )
        template_path.write_text(template_content, encoding="utf-8")
        defaults_path.write_text(
            "\n".join(
                [
                    "---",
                    "containerd_service_template_versions:",
                    *[f'  - "{version}"' for version in template_versions],
                    "",
                ]
            ),
            encoding="utf-8",
        )

        self.updater.ROOT = root
        self.updater.CONTAINERD_CONFIG = containerd_config
        self.updater.PPC64LE_CONTAINERD_CONFIG = ppc64le_config
        self.updater.CONTAINERD_SERVICE_TEMPLATE = template_path
        self.updater.CONTAINERD_DEFAULTS = defaults_path

        return template_path, defaults_path

    def test_write_updates_template_versions_for_equivalent_arch_pins(self):
        with tempfile.TemporaryDirectory(dir=SCRIPT.parent) as tmp:
            root = pathlib.Path(tmp)
            template_path, defaults_path = self.configure_fixture(
                root=root,
                generic_version="2.3.2",
                ppc64le_version="2.3.1",
                template_content="old service\n",
                template_versions=["2.3.2"],
            )

            services = {
                "2.3.2": "new service\n",
                "2.3.1": "new service\n",
            }

            def fake_fetch(version):
                return f"https://example.test/v{version}/containerd.service", services[
                    version
                ]

            with mock.patch.object(
                self.updater, "parse_args", return_value=SimpleNamespace(write=True)
            ), mock.patch.object(self.updater, "fetch_containerd_service", side_effect=fake_fetch):
                rc = self.updater.main()

            self.assertEqual(0, rc)
            self.assertEqual("new service\n", template_path.read_text(encoding="utf-8"))
            self.assertEqual(
                ["2.3.2", "2.3.1"],
                self.updater.service_template_versions(defaults_path),
            )

    def test_verify_accepts_equivalent_arch_pins_when_defaults_match(self):
        with tempfile.TemporaryDirectory(dir=SCRIPT.parent) as tmp:
            root = pathlib.Path(tmp)
            self.configure_fixture(
                root=root,
                generic_version="2.3.2",
                ppc64le_version="2.3.1",
                template_content="service unit\n",
                template_versions=["2.3.2", "2.3.1"],
            )

            services = {
                "2.3.2": "service unit\n",
                "2.3.1": "service unit\n",
            }

            def fake_fetch(version):
                return f"https://example.test/v{version}/containerd.service", services[
                    version
                ]

            with mock.patch.object(
                self.updater, "parse_args", return_value=SimpleNamespace(write=False)
            ), mock.patch.object(self.updater, "fetch_containerd_service", side_effect=fake_fetch):
                rc = self.updater.main()

            self.assertEqual(0, rc)

    def test_verify_rejects_equivalent_arch_pins_when_defaults_are_stale(self):
        with tempfile.TemporaryDirectory(dir=SCRIPT.parent) as tmp:
            root = pathlib.Path(tmp)
            self.configure_fixture(
                root=root,
                generic_version="2.3.2",
                ppc64le_version="2.3.1",
                template_content="service unit\n",
                template_versions=["2.3.2"],
            )

            services = {
                "2.3.2": "service unit\n",
                "2.3.1": "service unit\n",
            }

            def fake_fetch(version):
                return f"https://example.test/v{version}/containerd.service", services[
                    version
                ]

            stderr = io.StringIO()
            with mock.patch.object(
                self.updater, "parse_args", return_value=SimpleNamespace(write=False)
            ), mock.patch.object(
                self.updater, "fetch_containerd_service", side_effect=fake_fetch
            ), mock.patch("sys.stderr", stderr):
                rc = self.updater.main()

            self.assertEqual(1, rc)
            self.assertIn(
                "containerd_service_template_versions",
                stderr.getvalue(),
            )
            self.assertIn("supported containerd version pin(s)", stderr.getvalue())

    def test_verify_rejects_non_equivalent_arch_pins(self):
        with tempfile.TemporaryDirectory(dir=SCRIPT.parent) as tmp:
            root = pathlib.Path(tmp)
            self.configure_fixture(
                root=root,
                generic_version="2.3.2",
                ppc64le_version="2.3.1",
                template_content="service unit\n",
                template_versions=["2.3.2", "2.3.1"],
            )

            services = {
                "2.3.2": "service unit v2.3.2\n",
                "2.3.1": "service unit v2.3.1\n",
            }

            def fake_fetch(version):
                return f"https://example.test/v{version}/containerd.service", services[
                    version
                ]

            stderr = io.StringIO()
            with mock.patch.object(
                self.updater, "parse_args", return_value=SimpleNamespace(write=False)
            ), mock.patch.object(
                self.updater, "fetch_containerd_service", side_effect=fake_fetch
            ), mock.patch("sys.stderr", stderr):
                rc = self.updater.main()

            self.assertEqual(1, rc)
            self.assertIn("not equivalent", stderr.getvalue())

    def run_with_services(self, write, services):
        def fake_fetch(version):
            return f"https://example.test/v{version}/containerd.service", services[
                version
            ]

        stderr = io.StringIO()
        with mock.patch.object(
            self.updater, "parse_args", return_value=SimpleNamespace(write=write)
        ), mock.patch.object(
            self.updater, "fetch_containerd_service", side_effect=fake_fetch
        ), mock.patch("sys.stderr", stderr):
            rc = self.updater.main()
        return rc, stderr.getvalue()

    def test_verify_accepts_extra_versions_with_the_same_unit(self):
        with tempfile.TemporaryDirectory(dir=SCRIPT.parent) as tmp:
            self.configure_fixture(
                root=pathlib.Path(tmp),
                generic_version="2.3.1",
                ppc64le_version="2.3.1",
                template_content="service unit\n",
                template_versions=["2.3.1", "2.1.6"],
            )
            rc, _ = self.run_with_services(
                False, {"2.3.1": "service unit\n", "2.1.6": "service unit\n"}
            )
            self.assertEqual(0, rc)

    def test_verify_rejects_extra_versions_with_a_different_unit(self):
        with tempfile.TemporaryDirectory(dir=SCRIPT.parent) as tmp:
            self.configure_fixture(
                root=pathlib.Path(tmp),
                generic_version="2.3.1",
                ppc64le_version="2.3.1",
                template_content="service unit\n",
                template_versions=["2.3.1", "2.1.6"],
            )
            rc, stderr = self.run_with_services(
                False, {"2.3.1": "service unit\n", "2.1.6": "old service unit\n"}
            )
            self.assertEqual(1, rc)
            self.assertIn("lists 2.1.6", stderr)

    def test_write_keeps_matching_extra_versions_and_drops_others(self):
        with tempfile.TemporaryDirectory(dir=SCRIPT.parent) as tmp:
            _, defaults_path = self.configure_fixture(
                root=pathlib.Path(tmp),
                generic_version="2.3.1",
                ppc64le_version="2.3.1",
                template_content="service unit\n",
                template_versions=["2.3.1", "2.1.6", "1.7.27"],
            )
            rc, stderr = self.run_with_services(
                True,
                {
                    "2.3.1": "service unit\n",
                    "2.1.6": "service unit\n",
                    "1.7.27": "old service unit\n",
                },
            )
            self.assertEqual(0, rc)
            self.assertEqual(
                ["2.3.1", "2.1.6"],
                self.updater.service_template_versions(defaults_path),
            )
            self.assertIn("Dropping 1.7.27", stderr)


if __name__ == "__main__":
    unittest.main()
