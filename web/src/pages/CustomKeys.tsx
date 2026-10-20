import { useRef, useState, type FormEvent } from 'react'
import { ChevronLeft, ChevronRight } from 'lucide-react'
import { t } from '../i18n'
import { getPrefs, setPrefs, usePrefs } from '../prefs'
import { Confirm, Field } from '../components/ui'
import { controlChars, describeMacro, parseMacro } from '../terminal/macro'
import {
  builtinIds, builtins, defaultLayout, isBuiltin, layoutIn, longOf, mayLong, maxCustom, maxLabel, maxLayoutBytes, maxPinned, maxRow, layoutBytes,
  type BuiltinId, type KeyLayout, type LayoutKey,
} from '../terminal/layout'

// What is wrong with a sequence, for the user; '' when it is fine.
export function macroProblem(keys: string): string {
  const r = parseMacro(keys)
  return 'error' in r ? t(r.error, { step: r.at }) : ''
}

// A sequence in words ("Ctrl+B, then C"), or as written if it does not parse.
export function describeKeys(keys: string): string {
  const r = parseMacro(keys)
  return 'steps' in r ? describeMacro(r.steps, t('macro.then'), (s) => t('macro.textStep', { text: s })) : keys
}

type Group = 'pinned' | 'row'
type Pos = { group: Group; i: number }
const other = (g: Group): Group => (g === 'pinned' ? 'row' : 'pinned')
const sameKey = (a: LayoutKey | undefined, b: LayoutKey) => JSON.stringify(a) === JSON.stringify(b)
const current = (): KeyLayout => layoutIn(getPrefs().keyLayout, getPrefs().customKeys)
const ownCount = (l: KeyLayout) => [...l.pinned, ...l.row].filter((k) => !isBuiltin(k)).length

export function keyName(k: LayoutKey): string {
  if (!isBuiltin(k)) return k.label
  if (k.k === 'newline') return t('term.newlineKey')
  if (k.k === 'panel') return t('keybar.panel')
  return builtins[k.k].label
}

// The key bar's layout, the user's (terminal/layout.ts): the order of its
// keys, which stay on the left, which hide, keys of one's own, and what a
// long press sends. Kept with the other settings, so every device has it.
export function CustomKeysSection() {
  const prefs = usePrefs()
  const layout = layoutIn(prefs.keyLayout, prefs.customKeys)
  // The key being edited (or a new one): where it was and what it was.
  // Another device may change the layout meanwhile, so it must still be there.
  const [form, setForm] = useState<{ at: Pos | null; was: LayoutKey | null } | null>(null)
  const [conflict, setConflict] = useState(false)
  const [resetting, setResetting] = useState(false)
  const [label, setLabel] = useState('')
  const [keys, setKeys] = useState('')
  const [long, setLong] = useState('')
  const [tried, setTried] = useState(false)
  const [target, setTarget] = useState<'keys' | 'long'>('keys') // where the builder adds
  const labelRef = useRef<HTMLInputElement>(null)
  const keysRef = useRef<HTMLInputElement>(null)
  const longRef = useRef<HTMLInputElement>(null)
  const missing = builtinIds.filter((id) => ![...layout.pinned, ...layout.row].some((k) => isBuiltin(k) && k.k === id))
  const [adding, setAdding] = useState<BuiltinId | ''>('')

  // Saved with the other settings; the keys from before layouts are in it
  // now. One too large for the gateway is not saved.
  const [tooBig, setTooBig] = useState(false)
  const save = (l: KeyLayout): boolean => {
    if (layoutBytes(l) > maxLayoutBytes) {
      setTooBig(true)
      return false
    }
    setTooBig(false)
    setPrefs({ keyLayout: l, customKeys: [] })
    return true
  }
  const at = (l: KeyLayout, p: Pos) => l[p.group][p.i]
  // Changes act on the layout as it is now; one moved elsewhere meanwhile is
  // left alone.
  const change = (p: Pos, k: LayoutKey, f: (l: KeyLayout) => KeyLayout | null): boolean => {
    const l = current()
    if (!sameKey(at(l, p), k)) return false
    const next = f(l)
    return !!next && save(next)
  }
  const move = (p: Pos, k: LayoutKey, d: -1 | 1) =>
    change(p, k, (l) => {
      const g = [...l[p.group]]
      const j = p.i + d
      if (j < 0 || j >= g.length) return null
      ;[g[p.i], g[j]] = [g[j], g[p.i]]
      return { ...l, [p.group]: g }
    })
  const toOther = (p: Pos, k: LayoutKey) =>
    change(p, k, (l) => {
      const to = other(p.group)
      if (l[to].length >= (to === 'pinned' ? maxPinned : maxRow)) return null
      return { ...l, [p.group]: l[p.group].filter((_, j) => j !== p.i), [to]: [...l[to], k] } as KeyLayout
    })
  const remove = (p: Pos, k: LayoutKey) => {
    change(p, k, (l) => ({ ...l, [p.group]: l[p.group].filter((_, j) => j !== p.i) }))
    if (form?.at && form.at.group === p.group && form.at.i === p.i) close()
  }
  const addBuiltin = () => {
    if (!adding) return
    const l = current()
    if (l.row.length >= maxRow || [...l.pinned, ...l.row].some((k) => isBuiltin(k) && k.k === adding)) return
    save({ ...l, row: [...l.row, { k: adding }] })
    setAdding('')
  }

  const open = (p: Pos | null, k: LayoutKey | null) => {
    setForm({ at: p, was: k })
    setConflict(false)
    setTried(false)
    setLabel(k && !isBuiltin(k) ? k.label : '')
    setKeys(k && !isBuiltin(k) ? k.keys : '')
    setLong(k ? longOf(k) : '')
    setTarget(k && isBuiltin(k) ? 'long' : 'keys')
  }
  const close = () => {
    setForm(null)
    setConflict(false)
    setTried(false)
  }
  const editingBuiltin = form?.was && isBuiltin(form.was) ? form.was : null
  const longAllowed = !form?.was || mayLong(form.was)
  const l = label.trim()
  const labelProblem = editingBuiltin ? '' : !l ? t('customKeys.labelMissing') : [...l].length > maxLabel ? t('customKeys.labelLong') : controlChars.test(l) ? t('macro.control') : ''
  const keysProblem = editingBuiltin ? '' : keys.trim() ? macroProblem(keys) : t('macro.empty')
  const longProblem = long.trim() ? macroProblem(long) : ''
  const submit = (e: FormEvent) => {
    e.preventDefault()
    setTried(true)
    if (labelProblem) return labelRef.current?.focus()
    if (keysProblem) return keysRef.current?.focus()
    if (longProblem) return longRef.current?.focus()
    const lp = long.trim() ? long : ''
    let k: LayoutKey
    if (editingBuiltin) {
      // (The key's own default is not written down: it stays the default.)
      const def = longOf({ k: editingBuiltin.k })
      k = lp === def ? { k: editingBuiltin.k } : { k: editingBuiltin.k, long: lp }
    } else {
      k = lp ? { label: l, keys, long: lp } : { label: l, keys }
    }
    const cur = current()
    if (form?.at) {
      if (!sameKey(at(cur, form.at), form.was!)) return setConflict(true)
      const g = form.at.group
      if (!save({ ...cur, [g]: cur[g].map((x, j) => (j === form.at!.i ? k : x)) } as KeyLayout)) return // (the draft stays)
    } else {
      if (ownCount(cur) >= maxCustom || cur.row.length >= maxRow) return
      if (!save({ ...cur, row: [...cur.row, k] })) return
    }
    close()
  }
  const reset = () => {
    // Built-in keys as they were at first; one's own keys stay, in their order.
    const own = [...layout.pinned, ...layout.row].filter((k): k is { label: string; keys: string; long?: string } => !isBuiltin(k))
    const d = defaultLayout()
    save({ pinned: d.pinned, row: [d.row[0], ...own, ...d.row.slice(1)] })
  }
  const addToken = (tok: string) => {
    const [get, set, ref] = target === 'long' && longAllowed ? [long, setLong, longRef] : [keys, setKeys, keysRef]
    set(get.trim() ? get.trimEnd() + ' ' + tok : tok)
    ref.current?.focus()
  }

  // A key picked (as a chip) to act on; kept while it is where it was. The
  // buttons act on where it is now (taps may come faster than the page
  // redraws).
  const [sel, setSelState] = useState<{ at: Pos; key: LayoutKey } | null>(null)
  const selRef = useRef(sel)
  const setSel = (v: { at: Pos; key: LayoutKey } | null) => {
    selRef.current = v
    setSelState(v)
  }
  const picked = sel && sameKey(at(layout, sel.at), sel.key) ? sel : null
  const live = () => {
    const v = selRef.current
    return v && sameKey(at(current(), v.at), v.key) ? v : null
  }
  const chip = (g: Group) => (k: LayoutKey, i: number) => {
    const on = picked?.at.group === g && picked.at.i === i
    return (
      <button key={g + i} type="button" className={'kl-chip' + (isBuiltin(k) ? '' : ' own') + (longOf(k) || (isBuiltin(k) && k.k === 'ctrl') ? ' has-long' : '')}
        aria-pressed={on} onClick={() => setSel(on ? null : { at: { group: g, i }, key: k })}>
        {keyName(k)}
      </button>
    )
  }
  const actions = () => {
    if (!picked) return <p className="hint kl-tip">{t('keybar.pickTip')}</p>
    const { at: p, key: k } = picked
    const name = keyName(k)
    const lp = longOf(k)
    const what = !isBuiltin(k)
      ? t('keybar.sends', { what: describeKeys(k.keys) })
      : k.k === 'ctrl' ? t('keybar.ctrlDesc') : k.k === 'fn' ? t('keybar.fnDesc') : k.k === 'panel' ? t('keybar.panelDesc') : ''
    const to = other(p.group)
    const full = layout[to].length >= (to === 'pinned' ? maxPinned : maxRow)
    const step = (d: -1 | 1) => {
      const v = live()
      if (!v || v.at.i + d < 0 || v.at.i + d >= current()[v.at.group].length) return
      move(v.at, v.key, d)
      setSel({ at: { group: v.at.group, i: v.at.i + d }, key: v.key })
    }
    return (
      <div className="kl-act">
        <div className="kl-act-text">
          <span className="kl-act-name">{name}</span>
          {what && <span className="kl-sub">{what}</span>}
          {lp && <span className="kl-sub">{t('keybar.longOf', { what: describeKeys(lp) })}</span>}
        </div>
        <div className="kl-act-btns">
          <button type="button" className="icon-btn" disabled={p.i === 0} aria-label={t('keybar.up', { label: name })} title={t('keybar.upShort')} onClick={() => step(-1)}>
            <ChevronLeft size={18} />
          </button>
          <button type="button" className="icon-btn" disabled={p.i === layout[p.group].length - 1} aria-label={t('keybar.down', { label: name })} title={t('keybar.downShort')} onClick={() => step(1)}>
            <ChevronRight size={18} />
          </button>
          <button type="button" className="btn btn-ghost btn-sm" disabled={full} aria-label={t(p.group === 'pinned' ? 'keybar.toRow' : 'keybar.toPinned', { label: name })}
            onClick={() => {
              const v = live()
              if (!v) return
              const dest = other(v.at.group)
              const n = current()[dest].length
              if (toOther(v.at, v.key)) setSel({ at: { group: dest, i: n }, key: v.key })
            }}>
            {t(p.group === 'pinned' ? 'keybar.toRowShort' : 'keybar.toPinnedShort')}
          </button>
          {(!isBuiltin(k) || mayLong(k)) && (
            <button type="button" className="btn btn-ghost btn-sm" aria-label={t('customKeys.edit', { label: name })} onClick={() => open(p, k)}>
              {t('common.edit')}
            </button>
          )}
          <button type="button" className="btn btn-ghost btn-sm" aria-label={t(isBuiltin(k) ? 'keybar.hide' : 'customKeys.remove', { label: name })}
            onClick={() => {
              const v = live()
              if (v) remove(v.at, v.key)
              setSel(null)
            }}>
            {t(isBuiltin(k) ? 'keybar.hideShort' : 'common.delete')}
          </button>
        </div>
      </div>
    )
  }

  const keysPreview = !keysProblem && keys.trim() ? t('keybar.sends', { what: describeKeys(keys) }) : ''
  const longPreview = !longProblem && long.trim() ? t('keybar.longOf', { what: describeKeys(long) }) : ''
  const ownFull = ownCount(layout) >= maxCustom || layout.row.length >= maxRow
  return (
    <section className="section">
      <h2>{t('keybar.title')}</h2>
      <p className="hint">{t('keybar.lead')}</p>
      {(['pinned', 'row'] as Group[]).map((g) => (
        <div key={g} className="kl-group">
          <h3 id={'kl-' + g}>{t(g === 'pinned' ? 'keybar.pinned' : 'keybar.row')}</h3>
          {layout[g].length > 0 ? (
            <div className="kl-chips" role="group" aria-labelledby={'kl-' + g}>{layout[g].map(chip(g))}</div>
          ) : (
            <p className="hint">{t('keybar.groupEmpty')}</p>
          )}
        </div>
      ))}
      {actions()}
      {tooBig && <p className="err" role="alert">{t('keybar.tooBig')}</p>}
      <div className="kl-add">
        {missing.length > 0 && (
          <div className="kl-add-builtin">
            <select className="input" aria-label={t('keybar.addBuiltin')} value={adding} onChange={(e) => setAdding(e.target.value as BuiltinId | '')}>
              <option value="">{t('keybar.addBuiltin')}</option>
              {missing.map((id) => (
                <option key={id} value={id}>{keyName({ k: id })}</option>
              ))}
            </select>
            <button type="button" className="btn" disabled={!adding} onClick={addBuiltin}>{t('customKeys.add')}</button>
          </div>
        )}
        <div className="kl-add-actions">
          {!form && (
            <button type="button" className="btn" disabled={ownFull} onClick={() => open(null, null)}>{t('keybar.new')}</button>
          )}
          <button type="button" className="btn btn-ghost" onClick={() => setResetting(true)}>{t('keybar.reset')}</button>
        </div>
        {ownFull && !form && <p className="hint">{t('customKeys.full', { n: maxCustom })}</p>}
      </div>

      {form && (
        <form className="ck-form" onSubmit={submit} noValidate>
          <h3>{form.was ? t('customKeys.edit', { label: keyName(form.was) }) : t('keybar.new')}</h3>
          {conflict && <p className="err" role="alert">{t('customKeys.conflict')}</p>}
          {tooBig && <p className="err" role="alert">{t('keybar.tooBig')}</p>}
          {!editingBuiltin && (
            <>
              <Field label={t('customKeys.label')} htmlFor="ck-label">
                <input id="ck-label" ref={labelRef} className="input" value={label} placeholder={t('customKeys.labelPlaceholder')}
                  aria-invalid={tried && !!labelProblem} onChange={(e) => setLabel(e.target.value)} />
                {tried && labelProblem && <p className="err" role="alert">{labelProblem}</p>}
              </Field>
              <Field label={t('customKeys.keys')} htmlFor="ck-keys" hint={keysPreview || t('customKeys.keysHint')}>
                <input id="ck-keys" ref={keysRef} className="input mono" value={keys} placeholder="C-b c"
                  aria-invalid={(tried || !!keys.trim()) && !!keysProblem} autoCapitalize="none" autoCorrect="off" spellCheck={false}
                  onFocus={() => setTarget('keys')} onChange={(e) => setKeys(e.target.value)} />
                {(tried || keys.trim()) && keysProblem && <p className="err" role={tried ? 'alert' : undefined} aria-live="polite">{keysProblem}</p>}
              </Field>
            </>
          )}
          {longAllowed && (
            <Field label={t('keybar.long')} htmlFor="ck-long" optional hint={longPreview || t('keybar.longHint')}>
              <input id="ck-long" ref={longRef} className="input mono" value={long} placeholder="Esc Esc"
                aria-invalid={!!longProblem} autoCapitalize="none" autoCorrect="off" spellCheck={false}
                onFocus={() => setTarget('long')} onChange={(e) => setLong(e.target.value)} />
              {longProblem && <p className="err" aria-live="polite">{longProblem}</p>}
            </Field>
          )}
          <MacroBuilder onAdd={addToken} target={editingBuiltin ? null : longAllowed ? target : null} />
          <div className="ck-actions">
            <button type="button" className="btn" onClick={close}>{t('common.cancel')}</button>
            <button className="btn btn-primary">{form.was ? t('common.save') : t('customKeys.add')}</button>
          </div>
        </form>
      )}
      {resetting && (
        <Confirm title={t('keybar.reset')} body={t('keybar.resetBody')} confirm={t('keybar.reset')} onConfirm={reset} onClose={() => setResetting(false)} />
      )}
    </section>
  )
}

// Keys as picked rather than written: modifiers, then a key or a character,
// added as one step ("C-b"); text added quoted. Added to the field in use
// (target: which one, shown when there are two).
const pickKeys = ['Esc', 'Enter', 'Tab', 'Space', 'BSpace', 'Up', 'Down', 'Left', 'Right', 'Home', 'End', 'PgUp', 'PgDn', 'Del', 'F1', 'F2', 'F3', 'F4', 'F5', 'F6', 'F7', 'F8', 'F9', 'F10', 'F11', 'F12']
const pickChars = [...'abcdefghijklmnopqrstuvwxyz0123456789', '[', ']', '/', '-', '=', '.', ';', '`', '@', '^', '_', '?']
function MacroBuilder({ onAdd, target }: { onAdd: (tok: string) => void; target: 'keys' | 'long' | null }) {
  const [mods, setMods] = useState({ ctrl: false, alt: false, shift: false })
  const [key, setKey] = useState('')
  const [text, setText] = useState('')
  const toggle = (m: keyof typeof mods) => setMods((x) => ({ ...x, [m]: !x[m] }))
  const isChar = key.startsWith('c:')
  const ch = isChar ? key.slice(2) : ''
  // Shift goes with letters and keys; Ctrl with ASCII characters (as macro.ts takes them).
  const shiftOk = !isChar || /^[a-z]$/.test(ch)
  const add = () => {
    if (!key) return
    const prefix = (mods.ctrl ? 'C-' : '') + (mods.alt ? 'M-' : '') + (mods.shift && shiftOk ? 'S-' : '')
    onAdd(isChar ? (prefix ? prefix + ch : `'${ch}'`) : prefix + key)
    setKey('')
    setMods({ ctrl: false, alt: false, shift: false })
  }
  const addText = () => {
    if (!text) return
    onAdd('"' + text.replace(/["\\]/g, (c) => '\\' + c) + '"')
    setText('')
  }
  return (
    <div className="mb" role="group" aria-label={t('macro.builder')}>
      <div className="mb-title">
        {t('macro.builder')}
        {target && <span className="mb-target">{t(target === 'long' ? 'macro.toLong' : 'macro.toKeys')}</span>}
      </div>
      <div className="mb-row">
        {(['ctrl', 'alt', 'shift'] as const).map((m) => (
          <button key={m} type="button" className="btn mb-mod" aria-pressed={mods[m]} disabled={m === 'shift' && !shiftOk} onClick={() => toggle(m)}>
            {m === 'ctrl' ? 'Ctrl' : m === 'alt' ? 'Alt' : 'Shift'}
          </button>
        ))}
      </div>
      <div className="mb-row">
        <select className="input mb-key" aria-label={t('macro.key')} value={key} onChange={(e) => setKey(e.target.value)}>
          <option value="">{t('macro.key')}</option>
          <optgroup label={t('macro.keys')}>{pickKeys.map((k) => <option key={k} value={k}>{k}</option>)}</optgroup>
          <optgroup label={t('macro.chars')}>{pickChars.map((c) => <option key={c} value={'c:' + c}>{c}</option>)}</optgroup>
        </select>
        <button type="button" className="btn" disabled={!key} onClick={add}>{t('macro.add')}</button>
      </div>
      <div className="mb-row">
        <input className="input mb-text" aria-label={t('macro.text')} placeholder={t('macro.text')} value={text} onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              addText()
            }
          }} />
        <button type="button" className="btn" disabled={!text} onClick={addText}>{t('macro.addText')}</button>
      </div>
    </div>
  )
}
