"""Fail on style rules the project does not take (run by `make check`).

A bar along an element's left edge — to mark the current one, a kind, a
quote — is refused, whatever draws it: a left border of 2px or more, an
inset shadow on one side, a pseudo-element painted as a strip. The one
current or of a kind is told by a tint or the weight of its words. (A
1px rule between two panes is a divider, not a bar: allowed.)

Asked three times already (08-04, 10-21, 11-04): checked here so that it
cannot come back unseen.
"""
import glob, os, re, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FILES = glob.glob(os.path.join(ROOT, 'web', 'src', '**', '*.css'), recursive=True) + glob.glob(os.path.join(ROOT, 'web', 'src', '**', '*.tsx'), recursive=True)

RULES = [
    (re.compile(r'border-(?:left|inline-start)\s*:\s*(?:[2-9]|\d{2,})(?:\.\d+)?px'), 'a left border of 2px or more'),
    (re.compile(r'border-(?:left|inline-start)-width\s*:\s*(?:[2-9]|\d{2,})'), 'a left border of 2px or more'),
    (re.compile(r'borderLeft(?:Width)?\s*:\s*[\'"]?(?:[2-9]|\d{2,})'), 'a left border of 2px or more (inline style)'),
    (re.compile(r'inset\s+(?:[2-9]|\d{2,})(?:\.\d+)?px\s+0(?:px)?\s+0(?:px)?(?:\s|;|,|\))'), 'an inset shadow drawn as a left bar'),
]

bad = []
for f in FILES:
    with open(f, encoding='utf-8') as fh:
        for n, line in enumerate(fh, 1):
            for rx, what in RULES:
                if rx.search(line):
                    bad.append(f'{os.path.relpath(f, ROOT)}:{n}: {what}: {line.strip()[:120]}')
if bad:
    print('\n'.join(bad))
    sys.exit('left-edge bars are not used in this project: tint the element, or weight its words, instead')
print('styles: no left-edge bars')
