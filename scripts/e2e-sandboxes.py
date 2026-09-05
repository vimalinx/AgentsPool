#!/usr/bin/env python3
"""Root-only, isolated real systemd/bwrap integration test. No installed services."""
import base64
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def main():
    if os.geteuid() != 0:
        raise SystemExit('Run in a disposable Linux systemd VM/CI runner as root')
    binary = str(Path(sys.argv[1] if len(sys.argv) > 1 else 'dist/agentspool').resolve())
    for command in ('systemctl', 'systemd-run', 'bwrap', 'setpriv', 'mkfs.ext4', 'e2fsck', 'resize2fs'):
        if not shutil.which(command):
            raise SystemExit(f'Missing dependency: {command}')
    root = Path(tempfile.mkdtemp(prefix='agentspool-sandbox-ci-'))
    processes, handles, operations = [], [], []
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    url = f'http://127.0.0.1:{port}'
    admin, allocator, join = (secrets.token_urlsafe(32) for _ in range(3))
    base_env = {k: v for k, v in os.environ.items() if not k.startswith('AGENTSPOOL_')}

    def launch(name, args, extra):
        handle = open(root / f'{name}.log', 'w')
        handles.append(handle)
        process = subprocess.Popen([binary] + args, env=dict(base_env, **extra), stdout=handle, stderr=subprocess.STDOUT)
        processes.append(process)
        return process

    def api(method, path, body=None, token=admin, status=200):
        req = urllib.request.Request(url + path, data=None if body is None else json.dumps(body).encode(),
                                     method=method, headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json', 'X-AgentsPool-Resource-Lease-Token': token, 'X-AgentsPool-Sandbox-Token': token})
        try:
            response = urllib.request.urlopen(req, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = json.load(response)
            assert response.status == status, (method, path, response.status, data)
            if isinstance(data, dict) and 'operation' in data:
                operations.append(data['operation']['id'])
            return data

    def until(check, timeout=30):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            value = check()
            if value:
                return value
            time.sleep(.1)
        raise AssertionError('Timed out waiting for expected state')

    def wait(op, success=True):
        result = until(lambda: (lambda o: o if o['status'] in ('succeeded', 'failed', 'cancelled') else None)(
            api('GET', '/api/v1/sandbox-operations/' + op)['operation']))
        if success:
            assert result['status'] == 'succeeded', result
        return result

    ok = False
    try:
        control = launch('server', ['server', '--listen', f'127.0.0.1:{port}', '--data-dir', str(root / 'server')],
               {'AGENTSPOOL_ADMIN_TOKEN': admin, 'AGENTSPOOL_ALLOCATOR_TOKEN': allocator, 'AGENTSPOOL_JOIN_TOKEN': join})
        def health():
            try:
                return api('GET', '/healthz')
            except (OSError, urllib.error.URLError):
                return False
        until(health)
        helper = launch('helper', ['sandboxd', '--socket', str(root / 'helper.sock'), '--data-dir', str(root / 'sandboxes'),
                                  '--transfer-root', str(root / 'worker/transfers'), '--worker-user', 'nobody'], {})
        until(lambda: (root / 'helper.sock').exists())
        worker = launch('worker', ['worker', '--server', url, '--data-dir', str(root / 'worker'), '--name', 'sandbox-ci',
                                  '--poll-interval', '100ms', '--heartbeat-interval', '1s', '--max-concurrency', '2',
                                  '--graceful-timeout', '5s'],
                        {'AGENTSPOOL_JOIN_TOKEN': join, 'AGENTSPOOL_SANDBOX_SOCKET': str(root / 'helper.sock'),
                         'AGENTSPOOL_WORKER_DETECT_GPU': 'false'})
        devices = until(lambda: (lambda r: r if r.get('devices') else None)(api('GET', '/api/v1/devices')))
        node = devices['devices'][0]['id']
        resources = {'cpu_millis': 500, 'memory_mb': 128, 'disk_mb': 64}
        lease = api('POST', '/api/v1/resource-leases', {'node_id': node, 'expected_revision': devices['revision'],
                    'resources': resources, 'ttl_seconds': 0}, token=allocator, status=201)
        lid, lt = lease['lease']['id'], lease['lease_token']
        created = api('POST', '/api/v1/sandboxes', {'name': 'isolated-ci', 'resource_lease_id': lid,
                      'permissions': {'exec': True, 'data_read': True, 'data_write': True, 'network': False}}, token=lt, status=201)
        sid, st = created['sandbox']['id'], created['sandbox_token']
        path = '/api/v1/sandboxes/' + sid
        wait(created['operation']['id'])

        def execute(script):
            return api('POST', path + '/exec', {'command': ['/bin/sh', '-c', script]}, token=st, status=202)['operation']['id']

        op = execute('set -eu; echo persisted > /workspace/marker; test ! -e /etc/shadow; ! grep -Eq "^[^[:space:]]+[[:space:]]+00000000[[:space:]]" /proc/net/route; python3 -c \'print("x"*200000,end="")\'')
        result = wait(op)
        assert result.get('output_truncated') and len(result['stdout']) == 65536, result
        output, after = b'', 0
        while True:
            page = api('GET', f'/api/v1/sandbox-operations/{op}/logs?after={after}', token=st)
            output += base64.b64decode(page['data_base64'] or '')
            after = page['next_after']
            if after >= page['end_offset']:
                break
        assert output == b'x' * 200000, len(output)
        api('GET', f'/api/v1/sandbox-operations/{op}/logs', token=allocator, status=401)
        print('PASS durable full output, bounded preview, scoped logs, filesystem/network isolation', flush=True)

        api('POST', '/api/v1/resource-leases/' + lid + '/renew', {'ttl_seconds': 3}, token=lt)
        op = execute('echo live; sleep 5; echo renewed')
        until(lambda: api('GET', f'/api/v1/sandbox-operations/{op}/logs', token=st).get('data') == 'live\n')
        api('POST', '/api/v1/resource-leases/' + lid + '/renew', {'ttl_seconds': 0}, token=lt)
        assert wait(op)['stdout'] == 'live\nrenewed\n'
        print('PASS live logs and finite-to-unlimited renewal during real execution', flush=True)
        op = execute('echo before-restart; sleep 1; echo after-restart')
        until(lambda: api('GET', f'/api/v1/sandbox-operations/{op}/logs', token=st).get('data') == 'before-restart\n')
        control.terminate()
        control.wait(timeout=10)
        time.sleep(2)
        control = launch('server-restarted', ['server', '--listen', f'127.0.0.1:{port}', '--data-dir', str(root / 'server')],
                         {'AGENTSPOOL_ADMIN_TOKEN': admin, 'AGENTSPOOL_ALLOCATOR_TOKEN': allocator, 'AGENTSPOOL_JOIN_TOKEN': join})
        until(health)
        wait(op)
        assert api('GET', f'/api/v1/sandbox-operations/{op}/logs', token=st)['data'] == 'before-restart\nafter-restart\n'
        print('PASS execution finishing during control-plane downtime retains output and final result', flush=True)


        op = execute('echo running; sleep 120')
        until(lambda: api('GET', f'/api/v1/sandbox-operations/{op}/logs', token=st).get('data') == 'running\n')
        revision = api('GET', '/api/v1/devices')['revision']
        api('POST', path + '/resize', {'expected_revision': revision, 'resources': resources}, token=st, status=409)
        stop = api('POST', path + '/stop', {}, token=st, status=202)
        wait(stop['operation']['id'])
        assert wait(op, False)['status'] == 'cancelled'
        state = subprocess.check_output(['systemctl', 'show', 'agentspool-sbx-' + op.removeprefix('sbxop_').lower() + '.service', '-p', 'ActiveState', '--value'], text=True).strip()
        assert state in ('inactive', 'failed'), state
        device = api('GET', '/api/v1/devices')['devices'][0]
        assert device['reserved'] == {'cpu_millis': 0, 'memory_mb': 0, 'disk_mb': 64}, device['reserved']
        print('PASS stop acknowledgement, actual unit termination, compute released and disk retained', flush=True)
        # Backup refuses mounted data; restore verifies age authentication, file
        # checksums and ext4 before publishing a new node-local volume directory.
        volume_script = Path(__file__).with_name('sandbox-volume.py')
        identity, recipient, archive = root / 'age.key', root / 'age.recipient', root / 'volume.age'
        subprocess.run(['age-keygen', '-o', str(identity)], check=True, stderr=subprocess.DEVNULL)
        identity.chmod(0o600)
        recipient.write_bytes(subprocess.check_output(['age-keygen', '-y', str(identity)]))
        backup_args = ['python3', str(volume_script), 'backup', '--sandbox-id', sid,
                       '--data-dir', str(root / 'sandboxes'), '--archive', str(archive), '--recipient-file', str(recipient)]
        subprocess.run(backup_args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        saved = root / 'original-volume'
        (root / 'sandboxes' / sid).rename(saved)
        restore_args = ['python3', str(volume_script), 'restore', '--sandbox-id', sid,
                        '--data-dir', str(root / 'sandboxes'), '--archive', str(archive), '--identity-file', str(identity)]
        subprocess.run(restore_args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        assert subprocess.run(restore_args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode != 0
        print('PASS encrypted Worker volume backup, verified restore and overwrite refusal', flush=True)
        wait(api('POST', path + '/start', {}, token=st, status=202)['operation']['id'])
        wait(execute('test "$(cat /workspace/marker)" = persisted'))
        revision = api('GET', '/api/v1/devices')['revision']
        wait(api('POST', path + '/resize', {'expected_revision': revision, 'resources': dict(resources, disk_mb=96)}, token=st, status=202)['operation']['id'])
        wait(execute('test "$(cat /workspace/marker)" = persisted'))
        wait(api('DELETE', path, token=st, status=202)['operation']['id'])
        api('DELETE', '/api/v1/resource-leases/' + lid, token=lt)
        assert not (root / 'sandboxes' / sid).exists()
        print('PASS restart persistence, disk expansion, destruction and lease cleanup', flush=True)
        ok = True
    finally:
        for op in set(operations):
            subprocess.run(['systemctl', 'stop', 'agentspool-sbx-' + op.removeprefix('sbxop_').lower() + '.service'],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
        for process in reversed(processes):
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for mount in (root / 'sandboxes').glob('sbx_*/rootfs'):
            subprocess.run(['umount', str(mount)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for handle in handles:
            handle.close()
        if not ok:
            for log in root.glob('*.log'):
                print(log.name, log.read_text()[-12000:], file=sys.stderr)
        shutil.rmtree(root)


if __name__ == '__main__':
    main()
