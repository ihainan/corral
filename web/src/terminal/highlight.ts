import { createLowlight } from 'lowlight'
import bash from 'highlight.js/lib/languages/bash'
import c from 'highlight.js/lib/languages/c'
import cpp from 'highlight.js/lib/languages/cpp'
import css from 'highlight.js/lib/languages/css'
import diff from 'highlight.js/lib/languages/diff'
import dockerfile from 'highlight.js/lib/languages/dockerfile'
import go from 'highlight.js/lib/languages/go'
import ini from 'highlight.js/lib/languages/ini'
import java from 'highlight.js/lib/languages/java'
import javascript from 'highlight.js/lib/languages/javascript'
import json from 'highlight.js/lib/languages/json'
import makefile from 'highlight.js/lib/languages/makefile'
import markdown from 'highlight.js/lib/languages/markdown'
import python from 'highlight.js/lib/languages/python'
import rust from 'highlight.js/lib/languages/rust'
import sql from 'highlight.js/lib/languages/sql'
import typescript from 'highlight.js/lib/languages/typescript'
import xml from 'highlight.js/lib/languages/xml'
import yaml from 'highlight.js/lib/languages/yaml'
import type { Root } from 'hast'

// The grammars code is coloured with (loaded with the conversation view's
// first code, see Code.tsx). Their own aliases count (sh, ts, py, yml,
// html, toml…). lowlight gives a tree of tokens, never HTML: what is in the
// code stays text.
const lowlight = createLowlight({ bash, c, cpp, css, diff, dockerfile, go, ini, java, javascript, json, makefile, markdown, python, rust, sql, typescript, xml, yaml })
lowlight.registerAlias({ bash: ['shell', 'console', 'shellsession'] })

const MAX = 20000 // (characters) longer code shows as it is

export function tokens(code: string, lang: string): Root | null {
  const l = lang.toLowerCase()
  if (!l || code.length > MAX || !lowlight.registered(l)) return null
  try {
    return lowlight.highlight(l, code)
  } catch {
    return null
  }
}
