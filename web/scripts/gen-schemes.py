"""Generate web/src/terminal/schemes.ts from iTerm2-Color-Schemes (MIT).

Usage (from the repository root):
  mkdir -p /tmp/schemes && cd /tmp/schemes
  curl -sO https://raw.githubusercontent.com/mbadolato/iTerm2-Color-Schemes/master/LICENSE
  while read -r n; do curl -s -o "$n.json" "https://raw.githubusercontent.com/mbadolato/iTerm2-Color-Schemes/master/windowsterminal/$(python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1]))' "$n").json"; done < web/scripts/schemes.txt
  python3 -I web/scripts/gen-schemes.py /tmp/schemes web/scripts/schemes.txt web/src/terminal/schemes.ts

Every colour is checked to be #rrggbb before it is written out.
"""
import json, re, sys, os
src, listf, out = sys.argv[1], sys.argv[2], sys.argv[3]
HEX = re.compile(r'^#[0-9a-fA-F]{6}$')
KEYS = ['black','red','green','yellow','blue','purple','cyan','white','brightBlack','brightRed','brightGreen','brightYellow','brightBlue','brightPurple','brightCyan','brightWhite','background','foreground','cursorColor','selectionBackground']
RENAME = {
  'iTerm2 Solarized Dark': 'Solarized Dark', 'iTerm2 Solarized Light': 'Solarized Light',
  'Atom One Dark': 'One Dark', 'Atom One Light': 'One Light',
  'Rose Pine': 'Rosé Pine', 'Rose Pine Moon': 'Rosé Pine Moon', 'Rose Pine Dawn': 'Rosé Pine Dawn',
  'Everforest Dark Med': 'Everforest Dark', 'Everforest Light Med': 'Everforest Light',
  'Apple System Colors': 'Apple Dark', 'Apple System Colors Light': 'Apple Light',
  'Builtin Tango Dark': 'Tango Dark', 'Builtin Tango Light': 'Tango Light',
  'Night Owlish Light': 'Night Owl Light', 'TokyoNight Night': 'Tokyo Night', 'GitHub Dark Default': 'GitHub Dark', 'GitHub Light Default': 'GitHub Light',
}
def tokyo(n): return re.sub(r'^TokyoNight', 'Tokyo Night', n)
def lum(h):
    r, g, b = (int(h[i:i+2], 16) / 255 for i in (1, 3, 5))
    f = lambda c: c / 12.92 if c <= 0.03928 else ((c + 0.055) / 1.055) ** 2.4
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
items = []
for name in [l.strip() for l in open(listf) if l.strip()]:
    d = json.load(open(os.path.join(src, name + '.json')))
    for k in KEYS:
        v = d.get(k)
        if k in ('cursorColor', 'selectionBackground') and v is None: continue
        if not isinstance(v, str) or not HEX.match(v): raise SystemExit(f'bad {k} in {name}: {v!r}')
    disp = tokyo(RENAME.get(name, name))
    sid = re.sub(r'[^a-z0-9]+', '-', disp.lower().replace('é', 'e').replace('+', '-plus')).strip('-')
    t = {
      'background': d['background'], 'foreground': d['foreground'],
      'cursor': d.get('cursorColor', d['foreground']), 'cursorAccent': d['background'],
      'selectionBackground': d.get('selectionBackground', d['brightBlack']),
      'black': d['black'], 'red': d['red'], 'green': d['green'], 'yellow': d['yellow'], 'blue': d['blue'],
      'magenta': d['purple'], 'cyan': d['cyan'], 'white': d['white'],
      'brightBlack': d['brightBlack'], 'brightRed': d['brightRed'], 'brightGreen': d['brightGreen'],
      'brightYellow': d['brightYellow'], 'brightBlue': d['brightBlue'], 'brightMagenta': d['brightPurple'],
      'brightCyan': d['brightCyan'], 'brightWhite': d['brightWhite'],
    }
    t = {k: v.lower() for k, v in t.items()}
    items.append((disp, sid, lum(t['background']) < 0.18, t))
ids = [i[1] for i in items]
assert len(ids) == len(set(ids)), 'duplicate ids'
items.sort(key=lambda x: (not x[2], x[0].lower()))
lic = open(os.path.join(src, 'LICENSE')).read().strip()
with open(out, 'w') as f:
    f.write('// Generated from iTerm2-Color-Schemes (https://github.com/mbadolato/iTerm2-Color-Schemes),\n')
    f.write('// Windows Terminal format. Do not edit by hand. Original licence:\n//\n')
    for line in lic.splitlines(): f.write(('// ' + line).rstrip() + '\n')
    f.write('\nimport type { ITheme } from \'@xterm/xterm\'\n\n')
    f.write('export interface Scheme {\n  id: string\n  name: string\n  dark: boolean\n  theme: ITheme\n}\n\n')
    f.write('export const schemes: Scheme[] = [\n')
    for disp, sid, dark, t in items:
        body = ', '.join(f"{k}: '{v}'" for k, v in t.items())
        f.write(f"  {{ id: '{sid}', name: {json.dumps(disp, ensure_ascii=False)}, dark: {'true' if dark else 'false'}, theme: {{ {body} }} }},\n")
    f.write(']\n')
print(len(items), 'schemes;', sum(1 for i in items if i[2]), 'dark')
for disp, sid, dark, t in items: print(('D ' if dark else 'L ') + disp)
