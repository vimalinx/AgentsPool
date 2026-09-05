#!/usr/bin/env python3
"""Destructive installation qualification; refuses an existing AgentsPool install."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def pid(unit):
    return subprocess.check_output(['systemctl', 'show', unit, '-p', 'MainPID', '--value'], text=True).strip()


def wait_worker_online(node):
    token = Path('/etc/agentspool/admin.token').read_text().strip()
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        request = urllib.request.Request('http://127.0.0.1:8090/api/v1/devices', headers={'Authorization': 'Bearer ' + token})
        with urllib.request.urlopen(request, timeout=3) as response:
            devices = json.load(response)['devices']
        if any(device['id'] == node and device['status'] == 'online' for device in devices):
            return
        time.sleep(.1)
    raise AssertionError('Worker did not report online after restart')


def main():
    if os.geteuid() != 0 or not Path('/run/systemd/system').is_dir():
        raise SystemExit('Requires root on a disposable systemd Linux runner')
    for path in ('/etc/agentspool', '/usr/local/bin/agentspool', '/var/lib/agentspool/server', '/var/lib/agentspool/worker'):
        if Path(path).exists():
            raise SystemExit('Refusing existing installation: ' + path)
    custom_data = Path('/var/lib/agentspool-install-ci-worker')
    if custom_data.exists():
        raise SystemExit('Refusing existing custom test directory')
    archive = Path(sys.argv[1]).resolve()
    with tempfile.TemporaryDirectory(prefix='agentspool-install-ci-') as temp:
        root = Path(temp)
        # Qualification input is our freshly built archive, still reject paths/links.
        with tarfile.open(archive) as source:
            for member in source.getmembers():
                path = Path(member.name)
                if path.is_absolute() or '..' in path.parts or not (member.isdir() or member.isfile()):
                    raise SystemExit('Unsafe release archive entry')
            source.extractall(root)
        package = next(root.glob('AgentsPool-*'))
        installed = False
        try:
            run(['bash', 'scripts/install-server.sh'], cwd=package)
            installed = True
            env = {k: v for k, v in os.environ.items() if not k.startswith('AGENTSPOOL_')}
            env.update(AGENTSPOOL_SERVER='http://127.0.0.1:8090', AGENTSPOOL_JOIN_TOKEN_FILE='/etc/agentspool/join.token',
                       AGENTSPOOL_WORKER_NAME='install-ci', AGENTSPOOL_WORKER_DETECT_GPU='false')
            run(['bash', 'scripts/install-worker.sh'], cwd=package, env=env)
            names = ('agentspool-server', 'agentspool-worker', 'agentspool-sandboxd')
            before = {name: pid(name) for name in names}
            assert all(value != '0' for value in before.values()), before
            node = json.loads(Path('/var/lib/agentspool/worker/credentials.json').read_text())['node_id']
            config = digest('/etc/agentspool/server.env')
            run(['bash', 'scripts/install-server.sh'], cwd=package)
            assert all(pid(name) != before[name] for name in names), 'upgrade retained old running process'
            assert digest('/etc/agentspool/server.env') == config, 'upgrade replaced configured credentials'
            assert json.loads(Path('/var/lib/agentspool/worker/credentials.json').read_text())['node_id'] == node
            print('PASS root installation, shared executable restart and stable node/credential identity', flush=True)
            run(['systemctl', 'stop', 'agentspool-worker'])
            run(['bash', 'scripts/install-server.sh'], cwd=package)
            assert pid('agentspool-worker') == '0', 'upgrade started a deliberately stopped Worker'
            run(['systemctl', 'start', 'agentspool-worker'])
            wait_worker_online(node)
            print('PASS upgrade preserves deliberately stopped services', flush=True)

            # Use CLI discovery/first-run exactly as documented, including files.push/pull.
            client_env = dict(env, AGENTSPOOL_ALLOCATOR_TOKEN_FILE='/etc/agentspool/allocator.token')
            client_env.pop('AGENTSPOOL_JOIN_TOKEN_FILE', None)
            run(['bash', 'scripts/first-sandbox.sh', node, str(root / 'result.txt')], cwd=package, env=client_env)
            assert (root / 'result.txt').read_text() == 'HELLO FROM AGENTSPOOL\n'
            print('PASS documented first-sandbox CLI flow against installed services', flush=True)
            run(['systemctl', 'stop', 'agentspool-worker'])
            shutil.move('/var/lib/agentspool/worker', custom_data)
            worker_env = Path('/etc/agentspool/worker.env')
            worker_env.write_text(worker_env.read_text().replace('AGENTSPOOL_WORKER_DATA_DIR=/var/lib/agentspool/worker',
                                                                 'AGENTSPOOL_WORKER_DATA_DIR=' + str(custom_data)))
            run(['bash', 'scripts/install-worker.sh'], cwd=package, env=env)
            run(['systemctl', 'start', 'agentspool-worker'])
            wait_worker_online(node)
            run(['bash', 'scripts/first-sandbox.sh', node, str(root / 'custom-result.txt')], cwd=package, env=client_env)
            assert (root / 'custom-result.txt').read_text() == 'HELLO FROM AGENTSPOOL\n'
            print('PASS custom Worker state ownership, service permissions and helper transfer alignment', flush=True)


            stable = digest('/usr/local/bin/agentspool')
            broken = root / 'broken-agentspool'
            broken.write_text('#!/bin/sh\nif [ "$1" = version ]; then echo 0.4.0-alpha.999; exit 0; fi\nexit 42\n')
            broken.chmod(0o755)
            result = subprocess.run(['bash', 'scripts/install-server.sh', str(broken)], cwd=package)
            assert result.returncode != 0, 'broken upgrade unexpectedly succeeded'
            assert digest('/usr/local/bin/agentspool') == stable, 'binary rollback failed'
            assert all(pid(name) != '0' for name in names), 'rollback did not restore active services'
            print('PASS failed upgrade restores binary, configuration and stopped state snapshot', flush=True)

            # Uninstall must stop transient executions before waiting for Worker drain.
            run(['systemd-run', '--quiet', '--unit=agentspool-sbx-installfixture', '-p', 'TimeoutStopSec=2s', '/bin/sleep', '120'])
            run(['bash', 'scripts/uninstall.sh'], cwd=package, timeout=30)
            assert Path('/etc/agentspool/server.env').exists(), 'non-purge uninstall lost configuration'
            assert Path('/var/lib/agentspool/server/state.json').exists(), 'non-purge uninstall lost data'
            assert not Path('/usr/local/bin/agentspool').exists()
            assert not Path('/usr/lib/agentspool').exists()
            for suffix in ('service', 'timer'):
                for name in ('backup', 'tls-check'):
                    assert not Path(f'/etc/systemd/system/agentspool-{name}.{suffix}').exists()
            print('PASS active-unit uninstall ordering, timer/helper removal and preserved data', flush=True)
        finally:
            if installed:
                subprocess.run(['systemctl', 'stop', 'agentspool-sbx-installfixture.service'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                run(['bash', 'scripts/uninstall.sh', '--purge'], cwd=package, timeout=30)
                if custom_data.exists():
                    shutil.rmtree(custom_data)


if __name__ == '__main__':
    main()
