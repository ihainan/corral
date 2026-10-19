import { useRef, useState, type FormEvent } from 'react'
import { Pencil, Trash2 } from 'lucide-react'
import { t } from '../i18n'
import { maxCustomKeys, setPrefs, usePrefs, type CustomKey } from '../prefs'
import { Field } from '../components/ui'
import { controlChars, parseMacro } from '../terminal/macro'

// What is wrong with a sequence, for the user; '' when it is fine.
export function macroProblem(keys: string): string {
  const r = parseMacro(keys)
  return 'error' in r ? t(r.error, { step: r.at }) : ''
}

// The user's own keys on the terminal's key bar: a label and the sequence it
// sends. Kept with the other settings, so every device has them.
export function CustomKeysSection() {
  const prefs = usePrefs()
  const list = prefs.customKeys
  // The key being edited: where it was and what it was. Another device may
  // change the list meanwhile, so it must still be there to be saved.
  const [editing, setEditing] = useState<{ at: number; was: CustomKey } | null>(null)
  const [conflict, setConflict] = useState(false)
  const [label, setLabel] = useState('')
  const [keys, setKeys] = useState('')
  const [tried, setTried] = useState(false)
  const labelRef = useRef<HTMLInputElement>(null)
  const keysRef = useRef<HTMLInputElement>(null)
  // (Kept as typed: an escaped space at the end is part of it.)
  const problem = keys.trim() ? macroProblem(keys) : t('macro.empty')
  const l = label.trim()
  const labelProblem = !l ? t('customKeys.labelMissing') : [...l].length > 12 ? t('customKeys.labelLong') : controlChars.test(l) ? t('macro.control') : ''
  const same = (a: CustomKey, b: CustomKey) => a.label === b.label && a.keys === b.keys

  const reset = () => {
    setEditing(null)
    setConflict(false)
    setLabel('')
    setKeys('')
    setTried(false)
  }
  const submit = (e: FormEvent) => {
    e.preventDefault()
    setTried(true)
    if (labelProblem) return labelRef.current?.focus()
    if (problem) return keysRef.current?.focus()
    const k: CustomKey = { label: l, keys }
    if (editing) {
      // Changed or removed elsewhere since: not overwritten; the draft stays.
      const cur = list[editing.at]
      if (!cur || !same(cur, editing.was)) return setConflict(true)
      setPrefs({ customKeys: list.map((x, i) => (i === editing.at ? k : x)) })
    } else {
      if (list.length >= maxCustomKeys) return
      setPrefs({ customKeys: [...list, k] })
    }
    reset()
  }
  const remove = (i: number) => {
    setPrefs({ customKeys: list.filter((_, j) => j !== i) })
    if (editing?.at === i) reset()
    else if (editing && editing.at > i) setEditing({ at: editing.at - 1, was: editing.was })
  }
  const full = editing === null && list.length >= maxCustomKeys
  const labelErr = tried ? labelProblem : ''
  const keysErr = tried || keys.trim() ? problem : ''

  return (
    <section className="section">
      <h2>{t('customKeys.title')}</h2>
      <p className="hint">{t('customKeys.lead')}</p>
      {list.length > 0 && (
        <ul className="ck-list">
          {list.map((k, i) => (
            <li key={i} className={editing?.at === i ? 'editing' : undefined}>
              <span className="ck-label">{k.label}</span>
              <code className="ck-keys">{k.keys}</code>
              <button
                type="button"
                className="icon-btn"
                aria-label={t('customKeys.edit', { label: k.label })}
                onClick={() => {
                  setEditing({ at: i, was: k })
                  setConflict(false)
                  setLabel(k.label)
                  setKeys(k.keys)
                  setTried(false)
                }}
              >
                <Pencil size={16} />
              </button>
              <button type="button" className="icon-btn" aria-label={t('customKeys.remove', { label: k.label })} onClick={() => remove(i)}>
                <Trash2 size={16} />
              </button>
            </li>
          ))}
        </ul>
      )}
      {full ? (
        <p className="hint">{t('customKeys.full', { n: maxCustomKeys })}</p>
      ) : (
        <form className="ck-form" onSubmit={submit} noValidate>
          {conflict && (
            <p className="err" role="alert">
              {t('customKeys.conflict')}
            </p>
          )}
          <Field label={t('customKeys.label')} htmlFor="ck-label">
            <input
              id="ck-label"
              ref={labelRef}
              className="input"
              value={label}
              placeholder={t('customKeys.labelPlaceholder')}
              aria-invalid={!!labelErr}
              aria-describedby={labelErr ? 'ck-label-err' : undefined}
              onChange={(e) => setLabel(e.target.value)}
            />
            {labelErr && (
              <p id="ck-label-err" className="err" role="alert">
                {labelErr}
              </p>
            )}
          </Field>
          <Field label={t('customKeys.keys')} htmlFor="ck-keys" hint={keysErr ? undefined : t('customKeys.keysHint')}>
            <input
              id="ck-keys"
              ref={keysRef}
              className="input mono"
              value={keys}
              placeholder="C-b c"
              aria-invalid={!!keysErr}
              aria-describedby={keysErr ? 'ck-keys-err' : undefined}
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              onChange={(e) => setKeys(e.target.value)}
            />
            {keysErr && (
              <p id="ck-keys-err" className="err" role={tried ? 'alert' : undefined} aria-live={tried ? undefined : 'polite'}>
                {keysErr}
              </p>
            )}
          </Field>
          <div className="ck-actions">
            {editing !== null && (
              <button type="button" className="btn" onClick={reset}>
                {t('common.cancel')}
              </button>
            )}
            <button className="btn btn-primary">{editing === null ? t('customKeys.add') : t('common.save')}</button>
          </div>
        </form>
      )}
    </section>
  )
}
