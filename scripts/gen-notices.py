"""Generate web/public/THIRD-PARTY-NOTICES.txt: the licences of everything
the built program contains — the npm packages bundled into the web
interface, the Go modules compiled into the gateway — and the terminal
colour schemes (web/scripts/schemes-notice.txt, from gen-schemes.py).

Run after changing dependencies; `--check` (used by `make check`) fails
when the file is not up to date. Needs `pnpm install` in web/ and the Go
module cache (any `go build` fills it).
"""
import glob, json, os, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, 'web', 'public', 'THIRD-PARTY-NOTICES.txt')
# Type definitions are not part of the bundle.
NOT_BUNDLED = {'@types/react', 'csstype'}
# Build tools whose own code ends up in the bundle (Vite's module preload
# helper, Rolldown's runtime helpers), though they are development
# dependencies: each with the package it is resolved from.
BUILD_INJECTED = [('vite', None), ('rolldown', 'vite')]

# Every licence-like file of a package counts: some carry the notices of
# what they include themselves (e.g. LICENSE-3RD-PARTY.md).
PATTERNS = ('LICENSE*', 'LICENCE*', 'COPYING*', 'NOTICE*', 'THIRD-PARTY*', 'license*', 'OFL*')

def licence_text(d):
    files = sorted({f for pat in PATTERNS for f in glob.glob(os.path.join(d, pat)) if os.path.isfile(f)})
    if not files:
        sys.exit(f'no licence file in {d}')
    parts = []
    for path in files:
        with open(path, encoding='utf-8', errors='replace') as f:
            text = f.read().strip()
        parts.append(text if len(files) == 1 else f'[{os.path.basename(path)}]\n\n{text}')
    return '\n\n'.join(parts)

def npm_packages():
    env = dict(os.environ)
    out = subprocess.run(['pnpm', 'licenses', 'list', '--prod', '--json'], cwd=os.path.join(ROOT, 'web'),
                         capture_output=True, text=True, check=True, env=env).stdout
    pkgs = []
    for lic, entries in json.loads(out).items():
        for p in entries:
            if p['name'] in NOT_BUNDLED:
                continue
            for v, path in zip(p['versions'], p['paths']):
                pkgs.append((p['name'], v, lic, path))
    web = os.path.join(ROOT, 'web')
    for name, via in BUILD_INJECTED:
        base = os.path.realpath(os.path.join(web, 'node_modules', via)) if via else web
        d = os.path.dirname(subprocess.run(['node', '-p', f'require.resolve({json.dumps(name + "/package.json")}, {{paths: [{json.dumps(base)}]}})'],
                                           capture_output=True, text=True, check=True).stdout.strip())
        with open(os.path.join(d, 'package.json'), encoding='utf-8') as f:
            meta = json.load(f)
        pkgs.append((name, meta['version'], meta.get('license', ''), os.path.realpath(d)))
    return sorted(pkgs)

def go_modules():
    out = subprocess.run(['go', 'list', '-deps', '-f', '{{if not .Standard}}{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}', './cmd/corral'],
                         cwd=ROOT, capture_output=True, text=True, check=True).stdout
    mods = set()
    for line in out.splitlines():
        parts = line.split(' ', 2)
        if len(parts) == 3 and parts[1]:  # the main module has no version
            mods.add(tuple(parts))
    return sorted(mods)

def render():
    lines = ['Corral includes the following third-party software. Each part keeps its own licence, given in full below.', '']
    lines += ['Web interface (npm packages)', '============================', '']
    for name, ver, lic, path in npm_packages():
        lines += [f'{name} {ver} ({lic})', '-' * len(f'{name} {ver} ({lic})'), '', licence_text(path), '']
    lines += ['Gateway (Go modules)', '====================', '']
    for path, ver, d in go_modules():
        lines += [f'{path} {ver}', '-' * len(f'{path} {ver}'), '', licence_text(d), '']
    with open(os.path.join(ROOT, 'web', 'scripts', 'schemes-notice.txt'), encoding='utf-8') as f:
        lines.append(f.read().rstrip())
    return '\n'.join(lines) + '\n'

text = render()
if '--check' in sys.argv:
    with open(OUT, encoding='utf-8') as f:
        if f.read() != text:
            sys.exit('web/public/THIRD-PARTY-NOTICES.txt is out of date: run `make notices`')
    print('third-party notices up to date')
else:
    with open(OUT, 'w', encoding='utf-8') as f:
        f.write(text)
    print('wrote', OUT)
