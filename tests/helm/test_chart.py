import copy
import http.server
import json
import os
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "charts/argus"
HELM = os.environ.get("HELM", "helm")
DEFAULTS = CHART / "ci/default-values.yaml"
ALL_CHECKS = CHART / "ci/all-checks-values.yaml"
FULL_CONFIG = CHART / "ci/full-config-values.yaml"
CHECKS = ["pvcs", "hosts", "nodes", "postgres", "pgbackrest", "longhorn", "cronjobs", "certificates", "pki", "externalsecrets", "argocd"]


def run(*args, release="argus", namespace="argus"):
    return subprocess.run([HELM, "template", release, str(CHART), "-n", namespace, *args], text=True, capture_output=True)


def render(*args, release="argus", namespace="argus", values=DEFAULTS):
    result = run("-f", str(values), *args, release=release, namespace=namespace)
    if result.returncode != 0:
        raise AssertionError(result.stderr)
    return [d for d in yaml.safe_load_all(result.stdout) if d]


def render_values(values, *args):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml") as f:
        yaml.safe_dump(values, f)
        f.flush()
        return render("-f", f.name, *args)


def all_checks_values():
    return yaml.safe_load(ALL_CHECKS.read_text())


def kind(docs, name):
    return [d for d in docs if d["kind"] == name]


def config_of(docs):
    cm = next(d for d in kind(docs, "ConfigMap") if "config.json" in d["data"])
    return json.loads(cm["data"]["config.json"])


def check_config(config):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json") as f:
        json.dump(config, f)
        f.flush()
        subprocess.run([str(ROOT / "dist/argus"), "serve", "-config", f.name, "-check-config"], check=True, capture_output=True)


def rules(docs):
    out = []
    for role in kind(docs, "ClusterRole") + kind(docs, "Role"):
        for rule in role["rules"]:
            out.append((role["kind"], role["metadata"].get("namespace"), rule))
    return out


def permissions(docs):
    """Every (kind, namespace, group, resource, verb) a render grants."""
    return {(k, ns, rule["apiGroups"][0], resource, verb) for k, ns, rule in rules(docs) for resource in rule["resources"] for verb in rule["verbs"]}


class RecordingAPI(http.server.BaseHTTPRequestHandler):
    """A Kubernetes API that answers every read and records what was asked."""

    requests = []

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        RecordingAPI.requests.append(path)
        if path == "/version":
            body = {"gitVersion": "v1.0.0"}
        elif path.endswith("/configmaps/argus-state"):
            body = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "argus-state", "namespace": "argus", "resourceVersion": "1"},
                    "data": {"pgbackrest.json": '{"version":2,"entries":{}}'}}
        else:
            body = {"metadata": {"resourceVersion": "1"}, "items": []}
        data = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


def granted(docs, path):
    """Whether the rendered RBAC grants the read the binary made at path."""
    parts = path.strip("/").split("/")
    if parts[0] == "api":
        group, rest = "", parts[2:]
    else:
        group, rest = parts[1], parts[3:]
    namespace = None
    if rest[0] == "namespaces" and len(rest) > 2:
        namespace, rest = rest[1], rest[2:]
    resource = rest[0]
    name = rest[1] if len(rest) > 1 else None
    verb = "get" if name else "list"
    for kind_, role_namespace, rule in rules(docs):
        if kind_ == "Role" and role_namespace != namespace:
            continue
        if group in rule["apiGroups"] and resource in rule["resources"] and verb in rule["verbs"]:
            if name and rule.get("resourceNames") and name not in rule["resourceNames"]:
                continue
            return True
    return False


class ChartTests(unittest.TestCase):
    def test_rbac_grants_every_read_the_checks_make(self):
        """Run every Kubernetes check against a recording API with the
        all-checks configuration, then require each read to be granted by
        the chart's rendered RBAC as Argus's own service account."""
        values = all_checks_values()
        values["networkPolicy"]["enabled"] = False
        docs = render_values(values)
        config = config_of(docs)
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), RecordingAPI)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        origin = f"http://127.0.0.1:{server.server_port}"
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json") as f:
            json.dump(config, f)
            f.flush()
            for check in CHECKS:
                if check == "hosts":
                    continue
                RecordingAPI.requests = []
                subprocess.run([str(ROOT / "dist/argus"), "once", "-config", f.name, "-check", check, "-api-url", origin], capture_output=True)
                paths = [p for p in RecordingAPI.requests if p != "/version"]
                self.assertTrue(paths, check)
                for path in paths:
                    self.assertTrue(granted(docs, path), f"{check} reads {path}, which the chart does not grant")

    def test_security_strategy_probes_and_image(self):
        docs = render()
        deployment = kind(docs, "Deployment")[0]
        self.assertEqual(deployment["spec"]["replicas"], 1)
        self.assertEqual(deployment["spec"]["strategy"]["type"], "Recreate")
        pod = deployment["spec"]["template"]["spec"]
        container = pod["containers"][0]
        self.assertTrue(pod["securityContext"]["runAsNonRoot"])
        self.assertEqual(pod["securityContext"]["runAsUser"], 25120)
        self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
        self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
        self.assertEqual(container["securityContext"]["capabilities"]["drop"], ["ALL"])
        for probe in ("startupProbe", "readinessProbe", "livenessProbe"):
            self.assertEqual(container[probe]["httpGet"], {"path": "/healthz", "port": "api", "scheme": "HTTPS"})
        self.assertEqual(container["image"], f"ghcr.io/zekihan/argus:{(ROOT / 'VERSION').read_text().strip()}")
        self.assertEqual(container["args"], ["serve", "-config=/config/config.json"])
        service = kind(docs, "Service")[0]
        self.assertEqual(service["spec"]["selector"], deployment["spec"]["selector"]["matchLabels"])
        self.assertEqual(service["spec"]["ports"][0]["name"], "https")
        digest = "sha256:" + "a" * 64
        pod = kind(render("--set", f"image.digest={digest}"), "Deployment")[0]["spec"]["template"]["spec"]
        self.assertTrue(pod["containers"][0]["image"].endswith("@" + digest))

    def test_default_has_no_checks_and_no_permissions(self):
        docs = render()
        self.assertEqual(rules(docs), [])
        self.assertFalse(kind(docs, "ClusterRoleBinding") or kind(docs, "RoleBinding"))
        config = config_of(docs)
        self.assertEqual(config["checks"], {})
        self.assertEqual(config["state"], {"namespace": "argus", "name": "argus-state"})
        self.assertEqual(config["tls"], {"certFile": "/var/run/argus-tls/tls.crt", "keyFile": "/var/run/argus-tls/tls.key"})
        check_config(config)

    def test_all_checks_config_loads_in_the_binary(self):
        docs = render(values=ALL_CHECKS)
        config = config_of(docs)
        self.assertEqual(sorted(config["checks"]), sorted(CHECKS))
        self.assertNotIn("enabled", json.dumps(config))
        self.assertEqual(config["checks"]["hosts"]["privateKeyFile"], "/var/run/argus-ssh/ssh_private_key")
        self.assertEqual(config["checks"]["hosts"]["knownHostsFile"], "/config/known_hosts")
        check_config(config)
        known_hosts = next(d for d in kind(docs, "ConfigMap") if "config.json" in d["data"])["data"]["known_hosts"]
        self.assertIn("ssh-ed25519", known_hosts)

    def test_full_config_passes_every_field_through(self):
        values = yaml.safe_load(FULL_CONFIG.read_text())
        config = config_of(render(values=FULL_CONFIG))
        check_config(config)
        for key, value in values["config"].items():
            self.assertEqual(config[key], value)
        for name, section in values["checks"].items():
            expected = {k: v for k, v in section.items() if k != "enabled"}
            if name == "hosts":
                expected.update(privateKeyFile="/var/run/argus-ssh/ssh_private_key", knownHostsFile="/config/known_hosts")
            self.assertEqual(config["checks"][name], expected, name)

    def test_rbac_matches_the_design(self):
        docs = render(values=ALL_CHECKS)
        cluster = kind(docs, "ClusterRole")[0]["rules"]
        self.assertEqual(
            {(r["apiGroups"][0], res) for r in cluster for res in r["resources"]},
            {
                ("", "nodes"), ("", "persistentvolumeclaims"), ("", "nodes/stats"), ("storage.k8s.io", "volumeattachments"), ("metrics.k8s.io", "nodes"),
                ("postgres-operator.crunchydata.com", "postgresclusters"),
                ("cert-manager.io", "certificates"), ("cert-manager.io", "certificaterequests"),
                ("cert-manager.io", "issuers"), ("cert-manager.io", "clusterissuers"),
                ("trust.cert-manager.io", "bundles"),
                ("external-secrets.io", "externalsecrets"), ("external-secrets.io", "clusterexternalsecrets"),
                ("external-secrets.io", "secretstores"), ("external-secrets.io", "clustersecretstores"),
            },
        )
        self.assertTrue(all(r["verbs"] == ["get", "list"] for r in cluster if r["resources"] != ["nodes/stats"]))
        self.assertIn({"apiGroups": [""], "resources": ["nodes/stats"], "resourceNames": ["worker"], "verbs": ["get"]}, cluster)
        roles = {(r["metadata"]["namespace"], r["metadata"]["name"]): r["rules"] for r in kind(docs, "Role")}
        readers = {ns: {(rule["apiGroups"][0], res) for rule in rs for res in rule["resources"]} for (ns, name), rs in roles.items() if name.endswith("-reader")}
        self.assertEqual(readers, {
            "database": {("batch", "jobs"), ("batch", "cronjobs")},
            "app": {("batch", "jobs"), ("batch", "cronjobs")},
            "longhorn-system": {("longhorn.io", r) for r in ("volumes", "replicas", "nodes", "backuptargets", "systembackups")},
            "argocd": {("argoproj.io", "applications")},
        })
        for (ns, name), rs in roles.items():
            if name.endswith("-reader"):
                self.assertTrue(all(rule["verbs"] == ["list"] for rule in rs))
        state = roles[("argus", "argus-state")]
        self.assertEqual(state, [{"apiGroups": [""], "resources": ["configmaps"], "resourceNames": ["argus-state"], "verbs": ["get", "update"]}])
        for _, _, rule in rules(docs):
            self.assertNotIn("secrets", rule["resources"])
            self.assertNotIn("pods", rule["resources"])
            self.assertNotIn("nodes/proxy", rule["resources"])
            self.assertNotIn("*", rule["resources"] + rule["verbs"])
        bindings = kind(docs, "RoleBinding") + kind(docs, "ClusterRoleBinding")
        self.assertEqual(len(bindings), len(kind(docs, "Role")) + len(kind(docs, "ClusterRole")))
        for binding in bindings:
            self.assertEqual(binding["subjects"], [{"kind": "ServiceAccount", "name": "argus", "namespace": "argus"}])

    def test_a_disabled_check_gets_no_permissions(self):
        everything = permissions(render(values=ALL_CHECKS))
        only = {}
        for check in CHECKS:
            values = all_checks_values()
            values["checks"][check]["enabled"] = False
            docs = render_values(values)
            config = config_of(docs)
            self.assertNotIn(check, config["checks"])
            check_config(config)
            self.assertTrue(permissions(docs) <= everything, check)
            values = copy.deepcopy(all_checks_values())
            for other in CHECKS:
                values["checks"][other]["enabled"] = other == check
            docs = render_values(values)
            only[check] = permissions(docs)
            check_config(config_of(docs))
        # The union of single-check grants is exactly the all-checks grant.
        self.assertEqual(set().union(*only.values()), everything)
        self.assertEqual(only["hosts"], set())
        self.assertEqual(only["argocd"], {("Role", "argocd", "argoproj.io", "applications", "list")})
        self.assertEqual({p for p in everything if p[3] == "configmaps"}, {("Role", "argus", "", "configmaps", "get"), ("Role", "argus", "", "configmaps", "update")})
        self.assertTrue({p for p in everything if p[3] == "configmaps"} <= only["pgbackrest"])

    def test_pvc_inventory_required_before_permissions_render(self):
        for values in (
            {"checks": {"pvcs": {"enabled": True}}},
            {"checks": {"pvcs": {"enabled": True, "expectedNodes": ["worker"], "storageClasses": []}}},
            {"rbac": {"create": False}, "checks": {"pvcs": {"enabled": True}}},
        ):
            with self.assertRaises(AssertionError):
                render_values(values)
        values = all_checks_values()
        values["checks"]["pvcs"]["allowUnmounted"] = []
        self.assertFalse(any(p[3] == "volumeattachments" for p in permissions(render_values(values))))

    def test_bundle_rbac_only_when_bundles_are_expected(self):
        values = all_checks_values()
        self.assertTrue(any(p[2] == "trust.cert-manager.io" for p in permissions(render_values(values))))
        values["checks"]["pki"]["expectedBundles"] = []
        docs = render_values(values)
        self.assertFalse(any(p[2] == "trust.cert-manager.io" for p in permissions(docs)))
        self.assertTrue(any(p[3] == "certificaterequests" for p in permissions(docs)))

    def test_state_configmap_is_created_once_and_kept(self):
        docs = render()
        state = next(d for d in kind(docs, "ConfigMap") if d["metadata"]["name"] == "argus-state")
        self.assertEqual(json.loads(state["data"]["pgbackrest.json"]), {"version": 2, "entries": {}})
        self.assertEqual(state["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertEqual(state["metadata"]["annotations"]["argocd.argoproj.io/sync-options"], "Prune=false,Delete=false")

    def test_token_projected_only_into_the_argus_container(self):
        docs = render(values=ALL_CHECKS)
        pod = kind(docs, "Deployment")[0]["spec"]["template"]["spec"]
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertFalse(kind(docs, "ServiceAccount")[0]["automountServiceAccountToken"])
        self.assertEqual([c["name"] for c in pod["containers"]], ["argus"])
        mounts = {m["name"]: m for m in pod["containers"][0]["volumeMounts"]}
        self.assertEqual(mounts["kubernetes-api-access"]["mountPath"], "/var/run/secrets/kubernetes.io/serviceaccount")
        self.assertTrue(all(m["readOnly"] for m in mounts.values()))
        volumes = {v["name"]: v for v in pod["volumes"]}
        sources = volumes["kubernetes-api-access"]["projected"]["sources"]
        self.assertEqual(sources[0]["serviceAccountToken"]["path"], "token")
        self.assertEqual(volumes["api-auth"]["secret"], {"secretName": "chart-validation-only", "defaultMode": 0o440, "items": [{"key": "token", "path": "token"}]})
        self.assertEqual(volumes["tls"]["secret"]["secretName"], "argus-tls")
        self.assertEqual(volumes["ssh"]["secret"]["items"], [{"key": "ssh_private_key", "path": "ssh_private_key"}])
        self.assertFalse(kind(docs, "Secret"))
        config = config_of(docs)
        self.assertEqual(config["tokenFile"], mounts["api-auth"]["mountPath"] + "/token")

    def test_optional_certificate(self):
        self.assertFalse(kind(render(), "Certificate"))
        cert = kind(render(values=ALL_CHECKS), "Certificate")[0]["spec"]
        self.assertEqual(cert["secretName"], "argus-tls")
        self.assertEqual(cert["issuerRef"], {"name": "example-ca-issuer", "kind": "ClusterIssuer", "group": "cert-manager.io"})
        self.assertIn("argus.argus.svc", cert["dnsNames"])
        plain = render("--set", "tls.enabled=false")
        container = kind(plain, "Deployment")[0]["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["livenessProbe"]["httpGet"]["scheme"], "HTTP")
        self.assertNotIn("tls", config_of(plain))

    def test_network_policy(self):
        self.assertFalse(kind(render(), "NetworkPolicy"))
        policy = kind(render(values=ALL_CHECKS), "NetworkPolicy")[0]["spec"]
        self.assertEqual(policy["policyTypes"], ["Ingress", "Egress"])
        self.assertEqual(policy["ingress"], [{"from": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "monitoring"}}}], "ports": [{"port": 8443, "protocol": "TCP"}]}])
        egress = policy["egress"]
        self.assertEqual(egress[0]["to"], [{"ipBlock": {"cidr": "10.43.0.1/32"}}])
        self.assertEqual([p["port"] for p in egress[1]["ports"]], [53, 53])
        self.assertEqual(egress[2:], [
            {"to": [{"ipBlock": {"cidr": "192.0.2.10/32"}}], "ports": [{"port": 22, "protocol": "TCP"}]},
            {"to": [{"ipBlock": {"cidr": "192.0.2.11/32"}}], "ports": [{"port": 2222, "protocol": "TCP"}]},
        ])
        values = all_checks_values()
        values["checks"]["hosts"]["hosts"].append({"name": "named", "address": "host.example.internal"})
        with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml") as f:
            yaml.safe_dump(values, f)
            f.flush()
            result = run("-f", f.name)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("networkPolicy.sshPeers", result.stderr)
            policy = kind([d for d in yaml.safe_load_all(subprocess.run(
                [HELM, "template", "argus", str(CHART), "-n", "argus", "-f", f.name, "--set", "networkPolicy.sshPeers[0].cidr=198.51.100.0/24", "--set", "networkPolicy.sshPeers[0].port=22"],
                check=True, text=True, capture_output=True).stdout) if d], "NetworkPolicy")[0]["spec"]
            self.assertEqual(policy["egress"][-1]["to"], [{"ipBlock": {"cidr": "198.51.100.0/24"}}])

    def test_required_values_fail_closed(self):
        for args, message in (
            ((), "accessControl.existingSecret"),
            (("--set", "accessControl.existingSecret=x"), "tls.existingSecret"),
            (("-f", str(DEFAULTS), "--set", "checks.hosts.enabled=true", "--set", "checks.hosts.hosts[0].name=a", "--set", "checks.hosts.hosts[0].address=192.0.2.1"), "ssh.knownHosts"),
            (("-f", str(DEFAULTS), "--set", "checks.hosts.enabled=true", "--set", "ssh.knownHosts=x"), "ssh.existingSecret"),
            (("-f", str(DEFAULTS), "--set", "networkPolicy.enabled=true"), "networkPolicy.apiPeers"),
            (("-f", str(DEFAULTS), "--set", "tls.certificate.create=true"), "tls.certificate.issuerRef.name"),
        ):
            result = run(*args)
            self.assertNotEqual(result.returncode, 0, args)
            self.assertIn(message, result.stderr, args)
        for override in ("checks.unknown.enabled=true", "config.unknown=1", "checks.pki.rootSHA256=abc", "image.digest=latest",
                         "checks.cronjobs.itemz[0].name=x", "checks.hosts.hosts[0].adress=x", "checks.argocd.windowSeconds=1",
                         "checks.pgbackrest.clusters[0].namespace=db", "service.port=0"):
            self.assertNotEqual(run("-f", str(DEFAULTS), "--set", override).returncode, 0, override)

    def test_cluster_rbac_names_are_collision_resistant(self):
        first = kind(render(release="c", namespace="a-b", values=ALL_CHECKS), "ClusterRole")[0]
        second = kind(render(release="b-c", namespace="a", values=ALL_CHECKS), "ClusterRole")[0]
        self.assertNotEqual(first["metadata"]["name"], second["metadata"]["name"])
        self.assertLessEqual(len(first["metadata"]["name"]), 63)
        docs = render(values=ALL_CHECKS)
        role, binding = kind(docs, "ClusterRole")[0], kind(docs, "ClusterRoleBinding")[0]
        self.assertEqual(role["metadata"]["name"], binding["roleRef"]["name"])


if __name__ == "__main__":
    unittest.main()
