#!/usr/bin/env python3
"""Offline, encrypted backup/restore of one node-local persistent sandbox volume."""
import argparse
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile


def digest(path):
    with path.open('rb') as src:
        value = hashlib.sha256()
        for block in iter(lambda: src.read(1024 * 1024), b''):
            value.update(block)
        return value.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['backup', 'restore'])
    parser.add_argument('--sandbox-id', required=True)
    parser.add_argument('--data-dir', type=Path, default=Path('/var/lib/agentspool/sandboxes'))
    parser.add_argument('--archive', type=Path, required=True)
    parser.add_argument('--recipient-file', type=Path)
    parser.add_argument('--identity-file', type=Path)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error('run as root on the owning Worker, after sandboxes.stop has succeeded')
    if not re.fullmatch(r'sbx_[A-Za-z0-9_-]{1,120}', args.sandbox_id):
        parser.error('invalid sandbox ID')
    if not args.data_dir.is_absolute() or args.data_dir == Path('/') or args.data_dir.is_symlink():
        parser.error('data directory must be an absolute, non-root, non-symlink directory')
    for command in ('age', 'e2fsck'):
        if not shutil.which(command):
            parser.error(f'{command} is required')
    os.umask(0o077)
    args.data_dir.mkdir(mode=0o710, parents=True, exist_ok=True)
    root = args.data_dir / args.sandbox_id
    if root.is_symlink():
        parser.error('refusing symlink sandbox directory')
    locks = args.data_dir / 'locks'
    locks.mkdir(mode=0o700, exist_ok=True)
    lock_fd = os.open(locks / (args.sandbox_id + '.lock'), os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, 'w') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            parser.error('sandbox operation is active; stop it before backup/restore')
        # mountinfo escapes spaces and other special bytes with octal sequences.
        for line in Path('/proc/self/mountinfo').read_text().splitlines():
            mount = Path(re.sub(r'\\([0-7]{3})', lambda m: chr(int(m[1], 8)), line.split()[4]))
            if mount == root or root in mount.parents:
                parser.error('sandbox is mounted; wait for sandboxes.stop before backup/restore')
        with tempfile.TemporaryDirectory(prefix='.volume-', dir=args.data_dir) as directory:
            temp = Path(directory)
            plain = temp / 'volume.tar.gz'
            names = ('storage.ext4', 'metadata.json')
            if args.action == 'backup':
                if not args.recipient_file or not args.recipient_file.is_file():
                    parser.error('--recipient-file is required')
                if args.archive.exists():
                    parser.error('archive exists; refusing overwrite')
                for name in names:
                    if not stat.S_ISREG((root / name).lstat().st_mode):
                        parser.error(f'{name} must be a regular file')
                metadata = json.loads((root / 'metadata.json').read_text())
                if metadata.get('sandbox_id') != args.sandbox_id:
                    parser.error('metadata sandbox ID mismatch')
                subprocess.run(['e2fsck', '-fn', str(root / 'storage.ext4')], check=True, stdout=subprocess.DEVNULL)
                manifest = {'format': 1, 'sandbox_id': args.sandbox_id, 'node_id': metadata['node_id'],
                            'files': {name: {'sha256': digest(root / name), 'bytes': (root / name).stat().st_size} for name in names}}
                with tarfile.open(plain, 'w:gz') as archive:
                    for name in names:
                        archive.add(root / name, arcname=name, recursive=False)
                    data = json.dumps(manifest, sort_keys=True).encode()
                    info = tarfile.TarInfo('manifest.json')
                    info.size, info.mode = len(data), 0o600
                    archive.addfile(info, io.BytesIO(data))
                encrypted = temp / 'volume.age'
                subprocess.run(['age', '-R', str(args.recipient_file), '-o', str(encrypted), str(plain)], check=True)
                # O_EXCL keeps a racing backup from being overwritten; delete partial output on failure.
                fd = os.open(args.archive, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                try:
                    with os.fdopen(fd, 'wb') as out, encrypted.open('rb') as src:
                        shutil.copyfileobj(src, out, 1024 * 1024)
                        out.flush()
                        os.fsync(out.fileno())
                except BaseException:
                    args.archive.unlink()
                    raise
                print(f'Encrypted volume backup: {args.archive}; SHA256={digest(args.archive)}')
            else:
                if root.exists():
                    parser.error('sandbox directory exists; restore only to an empty location, never overwrite')
                if not args.identity_file or not args.identity_file.is_file() or args.identity_file.stat().st_mode & 0o077:
                    parser.error('--identity-file must name an owner-only private age identity')
                subprocess.run(['age', '-d', '-i', str(args.identity_file), '-o', str(plain), str(args.archive)], check=True)
                extracted = temp / 'restored'
                extracted.mkdir(mode=0o710)
                with tarfile.open(plain, 'r:gz') as archive:
                    members = archive.getmembers()
                    if len(members) != 3 or {m.name for m in members} != set(names) | {'manifest.json'}:
                        parser.error('unexpected or duplicate archive entries')
                    for member in members:
                        if not member.isfile() or member.size < 0:
                            parser.error('archive contains an unsafe entry type')
                        if member.name != 'storage.ext4' and member.size > 1024 * 1024:
                            parser.error('metadata is oversized')
                        with archive.extractfile(member) as src, (extracted / member.name).open('xb') as out:
                            shutil.copyfileobj(src, out, 1024 * 1024)
                manifest = json.loads((extracted / 'manifest.json').read_text())
                if manifest.get('format') != 1 or manifest.get('sandbox_id') != args.sandbox_id:
                    parser.error('backup identity/format mismatch')
                for name in names:
                    expected = manifest['files'][name]
                    if digest(extracted / name) != expected['sha256'] or (extracted / name).stat().st_size != expected['bytes']:
                        parser.error(f'{name} failed checksum/size verification')
                metadata = json.loads((extracted / 'metadata.json').read_text())
                if metadata['sandbox_id'] != args.sandbox_id or metadata['node_id'] != manifest['node_id']:
                    parser.error('metadata identity mismatch')
                subprocess.run(['e2fsck', '-fn', str(extracted / 'storage.ext4')], check=True, stdout=subprocess.DEVNULL)
                (extracted / 'manifest.json').unlink()
                # No live mutation: the helper acquires the same sandbox lock before creating this path.
                extracted.rename(root)
                print(f'Restored volume: {root}; owning node={manifest["node_id"]}. Restore matching control-plane state before start.')


if __name__ == '__main__':
    main()
