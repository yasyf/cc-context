import json
import os
import pathlib
import re
import shutil
import subprocess
import tempfile

outdir = pathlib.Path(os.environ['PROBE_OUTPUT']).resolve()
outdir.mkdir(parents=True, exist_ok=False)
env = dict(os.environ, GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL='/dev/null', GIT_AUTHOR_NAME='probe', GIT_AUTHOR_EMAIL='probe@example.invalid', GIT_COMMITTER_NAME='probe', GIT_COMMITTER_EMAIL='probe@example.invalid', GIT_TERMINAL_PROMPT='0', GIT_NO_LAZY_FETCH='1')


def run(argv, data=None, check=True):
    return subprocess.run(argv, input=data, env=env, capture_output=True, check=check)


def trace(repo, label, argv, payload):
    prefix = outdir / f'{repo.name}-{label}'
    raw = prefix.with_suffix('.strace')
    cmd = ['strace', '-f', '-yy', '-s', '512', '-e', 'trace=%file,read,getdents64', '-o', str(raw), 'git', '--git-dir=' + str(repo), *argv]
    p = run(cmd, payload, check=False)
    prefix.with_suffix('.stdout').write_bytes(p.stdout)
    prefix.with_suffix('.stderr').write_bytes(p.stderr)
    counts = {'openat': 0, 'newfstatat': 0, 'statx': 0, 'read': 0, 'getdents64': 0, 'head_read_calls': 0, 'head_read_bytes': 0, 'all_read_bytes': 0, 'head_getdents64': 0}
    head_paths = set()
    for line in raw.read_text().splitlines():
        call = re.search(r'\b(openat|newfstatat|statx|read|getdents64)\(', line)
        if call:
            counts[call[1]] += 1
        result = re.search(r'= (-?\d+)(?:\s|$)', line)
        returned = int(result[1]) if result else 0
        if call and call[1] == 'getdents64' and '/refs/heads>' in line:
            counts['head_getdents64'] += 1
        if call and call[1] == 'read' and returned > 0:
            counts['all_read_bytes'] += returned
            fd = re.search(r'read\(\d+<([^>]*)>', line)
            if fd and '/refs/heads/' in fd[1]:
                counts['head_read_calls'] += 1
                counts['head_read_bytes'] += returned
                head_paths.add(fd[1])
    counts['distinct_head_paths_read'] = len(head_paths)
    return {'label': label, 'argv': cmd, 'exit': p.returncode, 'stdout': p.stdout.decode(), 'stderr': p.stderr.decode(), 'counts': counts, 'trace': raw.name}


report = {'git_version': run(['git', '--version']).stdout.decode().strip(), 'strace_version': run(['strace', '--version']).stdout.decode().splitlines()[0], 'cases': [], 'limits': ['Filesystem syscall/read bytes, not physical disk or host recovery proof', 'System Git version recorded; production resolver selection needs separate semantic review', 'No product build or execution']}
with tempfile.TemporaryDirectory(prefix='ccx-exact-refs-') as tmp:
    for n in (32, 4096, 10000):
        repo = pathlib.Path(tmp) / f'loose-{n}'
        run(['git', 'init', '--bare', '--object-format=sha1', str(repo)])
        base = ['git', '--git-dir=' + str(repo)]
        tree = run([*base, 'mktree'], b'').stdout.strip()
        oid = run([*base, 'commit-tree', tree.decode()], b'probe\n').stdout.strip().decode()
        names = [f'refs/heads/b{i:05d}' for i in range(n)]
        run([*base, 'update-ref', '--stdin'], ''.join(f'create {name} {oid}\n' for name in names).encode())
        assert not (repo / 'packed-refs').exists()
        assert sum(p.is_file() for p in (repo / 'refs/heads').iterdir()) == n
        selected = names[:12]
        missing = ['refs/heads/missing-a', 'refs/heads/missing-b']
        payload = ''.join(name + '\n' for name in selected).encode()
        existing = [
            trace(repo, 'for-each-existing', ['for-each-ref', '--format=%(refname) %(objectname)', '--stdin'], payload),
            trace(repo, 'show-ref-existing', ['show-ref', '--verify', '--hash', '--', *selected], None),
            trace(repo, 'cat-file-existing', ['cat-file', '--batch-check=%(objectname) %(rest)'], ''.join(name + ' ' + name + '\n' for name in selected).encode()),
        ]
        assert existing[0]['exit'] == 0 and existing[0]['stdout'].splitlines() == [name + ' ' + oid for name in selected]
        assert existing[1]['exit'] == 0 and existing[1]['stdout'].splitlines() == [oid] * len(selected)
        assert existing[2]['exit'] == 0 and existing[2]['stdout'].splitlines() == [oid + ' ' + name for name in selected]
        mixed = [selected[0], missing[0], selected[1], missing[1], selected[2]]
        cases = existing + [
            trace(repo, 'for-each-missing', ['for-each-ref', '--format=%(refname) %(objectname)', '--stdin'], ''.join(name + '\n' for name in mixed).encode()),
            trace(repo, 'show-ref-missing', ['show-ref', '--verify', '--hash', '--', *mixed], None),
            trace(repo, 'cat-file-missing', ['cat-file', '--batch-check=%(objectname) %(rest)'], ''.join(name + ' ' + name + '\n' for name in mixed).encode()),
        ]
        assert cases[3]['exit'] == 0 and cases[3]['stdout'].splitlines() == [name + ' ' + oid for name in selected[:3]]
        assert cases[4]['exit'] != 0 and cases[4]['stdout'].splitlines() == [oid]
        assert cases[5]['exit'] == 0 and len(cases[5]['stdout'].splitlines()) == len(mixed)
        if n == 32:
            run([*base, 'symbolic-ref', 'refs/heads/symbolic', selected[0]])
            (repo / 'refs/heads/broken').write_text('not-an-oid\n')
            (repo / 'refs/heads/dangling-object').write_text('f' * 40 + '\n')
            for kind, ref in [('symbolic', 'refs/heads/symbolic'), ('broken', 'refs/heads/broken'), ('dangling-object', 'refs/heads/dangling-object')]:
                cases.extend([
                    trace(repo, 'for-each-' + kind, ['for-each-ref', '--format=%(refname) %(objectname)', '--stdin'], (ref + '\n').encode()),
                    trace(repo, 'show-ref-' + kind, ['show-ref', '--verify', '--hash', '--', ref], None),
                    trace(repo, 'cat-file-' + kind, ['cat-file', '--batch-check=%(objectname) %(rest)'], (ref + ' ' + ref + '\n').encode()),
                ])
        report['cases'].append({'n': n, 'oid': oid, 'requested_existing': len(selected), 'records': cases})
        shutil.rmtree(repo)
(outdir / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
print(json.dumps(report, indent=2))
