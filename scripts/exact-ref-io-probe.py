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


report = {'git_version': run(['git', '--version']).stdout.decode().strip(), 'strace_version': run(['strace', '--version']).stdout.decode().splitlines()[0], 'cases': [], 'limits': ['Prototype only; no product dependency or global installation', 'Filesystem syscall/read bytes, not physical disk throughput', 'Exact valid full-ref lookup; no prefix-pattern API contract']}


def compare(repo, label, wanted, expected):
    payload = ''.join(name + '\n' for name in wanted).encode()
    records = [
        trace(repo, 'for-each-' + label, ['for-each-ref', '--format=%(refname) %(objectname)', '--stdin'], payload),
        trace(repo, 'native-' + label, ['refs', 'read', '--stdin'], payload),
    ]
    for record in records:
        assert record['exit'] == 0, record
        rows = dict(line.split(' ', 1) for line in record['stdout'].splitlines())
        assert rows == expected, record
    return records


fixtures = [('sha1', 'loose', n) for n in (32, 4096, 10000)]
fixtures += [(fmt, layout, 32) for fmt in ('sha1', 'sha256') for layout in ('loose', 'packed', 'reftable') if (fmt, layout) != ('sha1', 'loose')]
with tempfile.TemporaryDirectory(prefix='ccx-native-refs-') as tmp:
    for object_format, layout, n in fixtures:
        repo = pathlib.Path(tmp) / f'{object_format}-{layout}-{n}'
        ref_format = 'reftable' if layout == 'reftable' else 'files'
        run(['git', 'init', '--bare', '--object-format=' + object_format, '--ref-format=' + ref_format, str(repo)])
        base = ['git', '--git-dir=' + str(repo)]
        tree = run([*base, 'mktree'], b'').stdout.strip().decode()
        oid = run([*base, 'commit-tree', tree], b'probe\n').stdout.strip().decode()
        names = [f'refs/heads/b{i:05d}' for i in range(n)]
        run([*base, 'update-ref', '--stdin'], ''.join(f'create {name} {oid}\n' for name in names).encode())
        if layout == 'packed':
            run([*base, 'pack-refs', '--all'])
            assert (repo / 'packed-refs').is_file()
        elif layout == 'loose':
            assert not (repo / 'packed-refs').exists()
            assert sum(p.is_file() for p in (repo / 'refs/heads').iterdir()) == n
        else:
            assert (repo / 'reftable').is_dir()
        selected = names[:12]
        records = compare(repo, 'existing', selected, {name: oid for name in selected})
        native = records[1]
        if layout == 'loose':
            assert native['counts']['distinct_head_paths_read'] == 12, native
            assert native['counts']['head_read_bytes'] == 12 * (len(oid) + 1), native
        wanted = [selected[0], 'refs/heads/missing-a', selected[1], 'refs/heads/missing-b', selected[2]]
        records += compare(repo, 'missing', wanted, {name: oid for name in selected[:3]})
        run([*base, 'symbolic-ref', 'refs/heads/symbolic', selected[0]])
        records += compare(repo, 'symbolic', ['refs/heads/symbolic'], {'refs/heads/symbolic': oid})
        run([*base, 'symbolic-ref', 'refs/heads/dangling-symbolic', 'refs/heads/missing-target'])
        records += compare(repo, 'dangling-symbolic', ['refs/heads/dangling-symbolic'], {})
        dangling = run([*base, 'commit-tree', tree], b'dangling-object\n').stdout.strip().decode()
        run([*base, 'update-ref', 'refs/heads/dangling-object', dangling])
        if layout == 'packed':
            run([*base, 'pack-refs', '--all'])
        object_path = repo / 'objects' / dangling[:2] / dangling[2:]
        assert object_path.is_file()
        object_path.unlink()
        records += compare(repo, 'dangling-object', ['refs/heads/dangling-object'], {'refs/heads/dangling-object': dangling})
        if layout != 'reftable':
            (repo / 'refs/heads').mkdir(parents=True, exist_ok=True)
            (repo / 'refs/heads/broken').write_text('not-an-oid\n')
            (repo / 'refs/heads/zero').write_text('0' * len(oid) + '\n')
            records += compare(repo, 'broken', ['refs/heads/broken'], {})
            records += compare(repo, 'zero', ['refs/heads/zero'], {})
        for label, payload in [('nul', b'refs/heads/b00000\0suffix\n'), ('escape', b'refs/../config\n'), ('root-ref', b'HEAD\n')]:
            bad = trace(repo, 'native-reject-' + label, ['refs', 'read', '--stdin'], payload)
            assert bad['exit'] != 0 and not bad['stdout'], bad
            records.append(bad)
        report['cases'].append({'n': n, 'object_format': object_format, 'layout': layout, 'oid': oid, 'requested_existing': len(selected), 'records': records})
        (outdir / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
        shutil.rmtree(repo)
print(json.dumps(report, indent=2))
