import { useEffect, useLayoutEffect, useState, type FormEvent, type ReactNode } from 'react'
import { Brain, ChevronDown, ChevronRight, Film, KeyRound, Languages, LogOut, Minus, Plus, ScrollText, Users } from 'lucide-react'
import { disablePush, enablePush, pushState, syncPush, type PushState } from '../push'
import { api, ApiError, errorText, type Me } from '../api'
import { getLang, hasKey, setLang, t, type Lang } from '../i18n'
import { setPrefs, usePrefs } from '../prefs'
import { linkHandler } from '../router'
import { rememberSettingsScroll, signOut, takeSettingsScroll, useSession } from '../app'
import { Confirm, Dialog, Field, LoadError, useToast } from '../components/ui'
import { CurrentScheme, SchemeDialog } from '../terminal/SchemePicker'
import { TwoStepSetting } from './TwoStep'
import { ssoErrorText } from './Auth'
import { CustomKeysSection } from './CustomKeys'
import { playChime } from '../terminal/chime'
import { setScreenReader, useScreenReader } from '../terminal/screenReader'
import { saveTranslateConfig, testTranslateConfig, TRANSLATE_LANGS, useTranslateConfig, type TranslateConfig, type TranslateMode } from '../terminal/thoughtTranslate'
import { Markdown } from '../terminal/markdown'

// As the terminal's menu sets it (one point at a time).
const FONT_MIN = 9
const isMac = /Mac|iPhone|iPad/.test(navigator.platform)
const FONT_MAX = 28

function Setting(props: { label: string; hint?: ReactNode; id?: string; children: ReactNode }) {
  return (
    <div className="setting">
      <div className="setting-label">
        <span id={props.id}>{props.label}</span>
        {props.hint && <span className="hint">{props.hint}</span>}
      </div>
      <div className="setting-control">{props.children}</div>
    </div>
  )
}

function Switch(props: { label: string; checked: boolean; onChange: (v: boolean) => void; hint?: ReactNode }) {
  return (
    <label className="setting">
      <span className="setting-label">
        <span>{props.label}</span>
        {props.hint && <span className="hint">{props.hint}</span>}
      </span>
      <span className="setting-control">
        <input type="checkbox" role="switch" className="switch" checked={props.checked} onChange={(e) => props.onChange(e.target.checked)} />
      </span>
    </label>
  )
}

function Seg<T extends string>(props: { labelledBy: string; value: T; options: [T, string][]; onChange: (v: T) => void }) {
  return (
    <div className="seg" role="group" aria-labelledby={props.labelledBy}>
      {props.options.map(([v, label]) => (
        <button key={v} type="button" aria-pressed={props.value === v} onClick={() => props.onChange(v)}>
          {label}
        </button>
      ))}
    </div>
  )
}

export function SettingsPage() {
  const { me, setMe, boot } = useSession()
  const prefs = usePrefs()
  const reader = useScreenReader()
  const toast = useToast()
  const [lang, setLangState] = useState<Lang>(getLang())
  const [profileDirty, setProfileDirty] = useState(false)
  const [translateDirty, setTranslateDirty] = useState(false)
  const [schemesOpen, setSchemesOpen] = useState(false)
  const [langAsk, setLangAsk] = useState<Lang | null>(null)
  const switchLang = (v: Lang) => {
    setLang(v)
    setLangState(v)
    location.reload()
  }
  // Back from a page opened here (phones): where the list was.
  useLayoutEffect(() => {
    const y = takeSettingsScroll()
    if (y) window.scrollTo(0, y)
  }, [])
  const morePage = (href: string) => (e: React.MouseEvent) => {
    rememberSettingsScroll()
    linkHandler(href)(e)
  }
  const stepFont = (d: number) => setPrefs({ fontSize: Math.min(FONT_MAX, Math.max(FONT_MIN, prefs.fontSize + d)) })

  return (
    <main className="page page-form page-settings">
      <div className="page-head"><h1>{t('settings.title')}</h1></div>

      {/* Phones have no sidebar: the recordings and admin pages open from
          here, first, not under every setting (ux1101 PG-07). */}
      {(boot.recordings || me?.role === 'admin') && (
        <section className="section phone-only">
          <h2>{t('settings.more')}</h2>
          <div className="settings-group">
            {boot.recordings && (
              <a className="setting setting-link" href="/recordings" onClick={morePage('/recordings')}>
                <Film size={18} strokeWidth={1.75} className="row-icon" />
                <span className="setting-label">{t('nav.recordings')}</span>
                <ChevronRight size={16} className="chev" />
              </a>
            )}
            {me?.role === 'admin' && (
              <>
                <a className="setting setting-link" href="/admin/users" onClick={morePage('/admin/users')}>
                  <Users size={18} strokeWidth={1.75} className="row-icon" />
                  <span className="setting-label">{t('nav.users')}</span>
                  <ChevronRight size={16} className="chev" />
                </a>
                <a className="setting setting-link" href="/admin/audit" onClick={morePage('/admin/audit')}>
                  <ScrollText size={18} strokeWidth={1.75} className="row-icon" />
                  <span className="setting-label">{t('nav.audit')}</span>
                  <ChevronRight size={16} className="chev" />
                </a>
              </>
            )}
          </div>
        </section>
      )}

      <section className="section">
        <h2>{t('settings.appearance')}</h2>
        <div className="settings-group">
          <Setting label={t('settings.theme')} id="theme-l">
            <Seg
              labelledBy="theme-l"
              value={prefs.theme}
              options={[['system', t('settings.themeSystem')], ['light', t('settings.themeLight')], ['dark', t('settings.themeDark')]]}
              onChange={(v) => setPrefs({ theme: v })}
            />
          </Setting>
          <Setting label={t('settings.language')} id="lang-l">
            <Seg
              labelledBy="lang-l"
              value={lang}
              options={[['zh-CN', '简体中文'], ['en', 'English']]}
              onChange={(v) => {
                // (The page reloads: a display name typed and not saved would go — ux1101 PG-55.)
                if (v !== lang && (profileDirty || translateDirty)) setLangAsk(v)
                else if (v !== lang) switchLang(v)
              }}
            />
          </Setting>
        </div>
      </section>

      <section className="section">
        <h2>{t('settings.terminal')}</h2>
        <div className="settings-group">
          <Setting label={t('settings.termTheme')} id="tt-l">
            <button type="button" className="btn scheme-btn" aria-labelledby="tt-l tt-v" onClick={() => setSchemesOpen(true)}>
              <span id="tt-v"><CurrentScheme /></span>
              <ChevronRight size={16} />
            </button>
          </Setting>
          <Setting label={t('settings.fontSize')} id="fs-l">
            <div className="stepper" role="group" aria-labelledby="fs-l">
              <button type="button" className="icon-btn" aria-label={t('settings.fontSmaller')} disabled={prefs.fontSize <= FONT_MIN} onClick={() => stepFont(-1)}>
                <Minus size={16} />
              </button>
              <span className="value" aria-live="polite">{prefs.fontSize}</span>
              <button type="button" className="icon-btn" aria-label={t('settings.fontLarger')} disabled={prefs.fontSize >= FONT_MAX} onClick={() => stepFont(1)}>
                <Plus size={16} />
              </button>
            </div>
          </Setting>
          <Switch label={t('settings.webgl')} hint={t('settings.webglHint')} checked={prefs.webgl} onChange={(v) => setPrefs({ webgl: v })} />
          <Switch label={t('settings.screenReader')} hint={t('settings.screenReaderHint')} checked={reader} onChange={setScreenReader} />
          <Switch label={t('settings.remoteClipboard')} hint={t('settings.remoteClipboardHint')} checked={prefs.remoteClipboard} onChange={(v) => setPrefs({ remoteClipboard: v })} />
        </div>
      </section>

      {schemesOpen && <SchemeDialog onClose={() => setSchemesOpen(false)} />}
      {langAsk && (
        <Confirm title={t('settings.langTitle')} body={t('settings.langUnsaved')} confirm={t('settings.langSwitch')} onClose={() => setLangAsk(null)} onConfirm={() => switchLang(langAsk)} />
      )}

      <section className="section">
        <h2>{t('settings.keyboard')}</h2>
        <div className="settings-group">
          <Setting label={t('settings.composerEnter')} hint={t('settings.composerEnterHint', { mod: isMac ? t('settings.modEnterMac') : t('settings.modEnterOther') })} id="ce-l">
            <Seg
              labelledBy="ce-l"
              value={prefs.composerEnter}
              options={[['send', t('settings.composerEnterSend')], ['newline', t('settings.composerEnterNewline')]]}
              onChange={(v) => setPrefs({ composerEnter: v })}
            />
          </Setting>
          {/* (Option is a Mac key: elsewhere the setting means nothing — ux1101 PG-23.) */}
          {isMac && <Switch label={t('settings.optionIsMeta')} hint={t('settings.optionIsMetaHint')} checked={prefs.optionIsMeta} onChange={(v) => setPrefs({ optionIsMeta: v })} />}
        </div>
      </section>

      <section className="section">
        <h2>{t('settings.agents')}</h2>
        <div className="settings-group">
          {/* (Pinned at the top of the agents page: let go of all at once, those out of sight too.) */}
          <Setting label={t('settings.pinned')} hint={prefs.pinnedAgents.length ? t('settings.pinnedHint', { n: prefs.pinnedAgents.length }) : t('settings.pinnedNone')} id="pin-l">
            <button type="button" className="btn" aria-describedby="pin-l" disabled={!prefs.pinnedAgents.length} onClick={() => setPrefs({ pinnedAgents: [] })}>
              {t('settings.unpinAll')}
            </button>
          </Setting>
        </div>
      </section>

      <TranslateSection onDirty={setTranslateDirty} />

      {/* (A sound on this device when an agent finishes, needs you, or a program rings: terminal/chime.ts.) */}
      <section className="section">
        <h2>{t('settings.chime')}</h2>
        <div className="settings-group">
          <Setting label={t('settings.chimeWhen')} hint={t('settings.chimeWhenHint')} id="chime-l">
            <Seg
              labelledBy="chime-l"
              value={prefs.chime}
              options={[['off', t('settings.chimeOff')], ['done', t('settings.chimeDone')], ['all', t('settings.chimeAll')]]}
              onChange={(v) => setPrefs({ chime: v })}
            />
          </Setting>
          {prefs.chime !== 'off' && (
            <>
              <Setting label={t('settings.chimeTone')} id="chime-tone-l">
                <div className="settings-inline">
                  <Seg
                    labelledBy="chime-tone-l"
                    value={prefs.chimeTone}
                    options={[['bell', t('settings.chimeToneBell')], ['soft', t('settings.chimeToneSoft')], ['ping', t('settings.chimeTonePing')]]}
                    onChange={(v) => (setPrefs({ chimeTone: v }), playChime(v))}
                  />
                  <button type="button" className="btn" onClick={() => playChime(prefs.chimeTone)}>
                    {t('settings.chimeTry')}
                  </button>
                </div>
              </Setting>
              <Switch label={t('settings.chimeBells')} hint={t('settings.chimeBellsHint')} checked={prefs.chimeBells} onChange={(v) => setPrefs({ chimeBells: v })} />
            </>
          )}
        </div>
      </section>

      <section className="section">
        <h2>{t('settings.touch')}</h2>
        <div className="settings-group">
          <Switch label={t('settings.keybar')} checked={prefs.keybar} onChange={(v) => setPrefs({ keybar: v })} />
          {prefs.keybar && <Switch label={t('settings.tapKeyboard')} hint={t('settings.tapKeyboardHint')} checked={prefs.tapKeyboard} onChange={(v) => setPrefs({ tapKeyboard: v })} />}
        </div>
      </section>

      <CustomKeysSection />

      <PushSection />

      <section className="section">
        <h2>{t('settings.account')}</h2>
        <div className="settings-group">
          <ProfileSetting onDirty={setProfileDirty} />
          <PasswordSetting />
          {boot?.oidc?.enabled && <SSOSetting />}
          <TwoStepSetting />
          {/* Phones have no sidebar: sign-out is the account's last row. (Not
              in red: signing out loses nothing — ux1101 PG-56.) */}
          <button className="setting setting-link phone-only" onClick={() => signOut(setMe, toast)}>
            <LogOut size={18} strokeWidth={1.75} className="row-icon" />
            <span className="setting-label">
              {t('nav.signOut')}
              <span className="hint">{me?.username}</span>
            </span>
          </button>
        </div>
      </section>

      <p className="notices">
        <a href="/THIRD-PARTY-NOTICES.txt" target="_blank" rel="noopener">{t('settings.notices')}</a>
      </p>
    </main>
  )
}

const isLocalTranslateUrl = (url: string) => /^https?:\/\/(localhost|127\.0\.0\.1|\[::1\])(?::\d+)?\b/i.test(url.trim())

const previewSampleFor = (targetLang: string) =>
  targetLang === 'en'
    ? '先检查缓存命中情况，再返回结果。\n\n- 确认 `cache.get(key)` 返回的是有效条目。\n- 如果缓存已过期，则回退请求上游接口。'
    : 'Checking the cache lookup before returning a response.\n\n- Verify `cache.get(key)` returns a fresh entry.\n- Fall back to the upstream API if the entry has expired.'

const previewTitleOf = (text: string) => {
  const line = text.split('\n').find((l) => l.trim()) ?? ''
  return line.replace(/^\s*(?:Thinking\s*:?\s*)?/i, '').replace(/^[-*#>\s]+/, '').replace(/`([^`]+)`/g, '$1').trim()
}

function TranslateSection({ onDirty }: { onDirty: (d: boolean) => void }) {
  const toast = useToast()
  const { config } = useTranslateConfig()
  const [open, setOpen] = useState(false)
  const [saving, setSaving] = useState(false)

  useEffect(() => onDirty(false), [onDirty])

  const toggleEnabled = async (next: boolean) => {
    if (saving) return
    setSaving(true)
    try {
      const saved = await saveTranslateConfig({ enabled: next })
      if (saved.enabled && !saved.has_key && !isLocalTranslateUrl(saved.base_url)) {
        toast(t('convo.translate.needKey'))
        setOpen(true)
      } else {
        toast(t(saved.enabled ? 'convo.translate.enabled' : 'convo.translate.disabled'))
      }
    } catch (err) {
      toast(errorText(err), true)
    } finally {
      setSaving(false)
    }
  }

  const modeLabel = config?.mode === 'summarize' ? t('settings.translateModeSummarize') : t('settings.translateModeTranslate')
  const langLabel = TRANSLATE_LANGS.find((l) => l.code === (config?.target_lang || 'zh-CN'))?.label ?? (config?.target_lang || 'zh-CN')
  const modelName = config?.model || 'deepseek-flash'
  const keyStatus =
    !config || config.has_key || isLocalTranslateUrl(config.base_url)
      ? t('settings.translateKeyReady')
      : t('settings.translateKeyMissing')
  const summaryHint = `${modeLabel} · ${langLabel} · ${modelName} · ${keyStatus}`

  return (
    <section className="section">
      <h2>{t('settings.translate')}</h2>
      <div className="settings-group">
        <Switch
          label={t('settings.translateEnable')}
          hint={t('settings.translateEnableHint')}
          checked={!!config?.enabled}
          onChange={(v) => void toggleEnabled(v)}
        />
        <Setting label={t('settings.translateConfig')} hint={summaryHint} id="tr-cfg-l">
          <button type="button" className="btn" aria-labelledby="tr-cfg-l" onClick={() => setOpen(true)}>
            {t('settings.translateConfigure')}
          </button>
        </Setting>
      </div>
      {open && <TranslateConfigDialog config={config} onClose={() => setOpen(false)} />}
    </section>
  )
}

function TranslateConfigDialog({ config, onClose }: { config: TranslateConfig | null; onClose: () => void }) {
  const toast = useToast()
  const [mode, setMode] = useState<TranslateMode>(config?.mode === 'summarize' ? 'summarize' : 'translate')
  const [targetLang, setTargetLang] = useState(config?.target_lang || 'zh-CN')
  const [baseUrl, setBaseUrl] = useState(config?.base_url || 'https://api.deepseek.com/v1')
  const [model, setModel] = useState(config?.model || 'deepseek-flash')
  const [apiKey, setApiKey] = useState('')
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [previewOpen, setPreviewOpen] = useState(true)
  const [showOrig, setShowOrig] = useState(false)
  const [testState, setTestState] = useState<{ ok: boolean; mode?: TranslateMode; latency?: number; original?: string; translated?: string; error?: string } | null>(null)

  const dirty =
    !!config &&
    (mode !== (config.mode === 'summarize' ? 'summarize' : 'translate') || targetLang !== config.target_lang || baseUrl.trim() !== config.base_url || model.trim() !== config.model || apiKey.trim() !== '')

  const onLangChange = (next: string) => {
    setTargetLang(next)
    setTestState(null)
    setShowOrig(false)
  }

  const onModeChange = (next: TranslateMode) => {
    setMode(next)
    setTestState(null)
    setShowOrig(false)
  }

  const save = async (e: FormEvent) => {
    e.preventDefault()
    if (saving) return
    setSaving(true)
    try {
      const patch: Parameters<typeof saveTranslateConfig>[0] = {
        mode,
        target_lang: targetLang,
        base_url: baseUrl.trim(),
        model: model.trim(),
      }
      if (apiKey.trim()) patch.api_key = apiKey.trim()
      await saveTranslateConfig(patch)
      toast(t('settings.saved'))
      onClose()
    } catch (err) {
      toast(errorText(err), true)
    } finally {
      setSaving(false)
    }
  }

  const clearKey = async () => {
    if (saving) return
    setSaving(true)
    try {
      await saveTranslateConfig({ clear_key: true })
      setApiKey('')
      toast(t('settings.saved'))
    } catch (err) {
      toast(errorText(err), true)
    } finally {
      setSaving(false)
    }
  }

  const runTest = async () => {
    if (testing) return
    setTesting(true)
    setPreviewOpen(true)
    setShowOrig(false)
    try {
      const res = await testTranslateConfig({
        mode,
        target_lang: targetLang,
        base_url: baseUrl.trim(),
        model: model.trim(),
        ...(apiKey.trim() ? { api_key: apiKey.trim() } : {}),
      })
      setTestState({
        ok: true,
        mode: res.mode || mode,
        latency: res.latency_ms,
        original: res.original || previewSampleFor(targetLang),
        translated: res.sample,
      })
    } catch (err) {
      setTestState({ ok: false, error: errorText(err) })
    } finally {
      setTesting(false)
    }
  }

  const originalText = testState?.original || previewSampleFor(targetLang)
  const translatedText = testState?.ok ? testState.translated : undefined
  const shownText = translatedText && !showOrig ? translatedText : originalText
  const headTitle = previewTitleOf(shownText)

  return (
    <Dialog
      wide
      title={t('settings.translateDialogTitle')}
      sub={t('settings.translateDialogSub')}
      icon={<Languages size={18} />}
      guard={dirty}
      busy={saving}
      onClose={onClose}
    >
      <form className="stack" onSubmit={save}>
        <Field label={t('settings.translateMode')} hint={t(mode === 'summarize' ? 'settings.translateModeSummarizeHint' : 'settings.translateModeTranslateHint')}>
          <div className="seg" role="radiogroup" aria-label={t('settings.translateMode')}>
            {(['translate', 'summarize'] as const).map((m) => (
              <button key={m} type="button" role="radio" aria-checked={mode === m} aria-pressed={mode === m} onClick={() => onModeChange(m)}>
                {t(m === 'summarize' ? 'settings.translateModeSummarize' : 'settings.translateModeTranslate')}
              </button>
            ))}
          </div>
        </Field>
        <Field label={t('settings.translateLang')}>
          <select className="select" value={targetLang} onChange={(e) => onLangChange(e.target.value)}>
            {TRANSLATE_LANGS.map((l) => (
              <option key={l.code} value={l.code}>{l.label}</option>
            ))}
          </select>
        </Field>
        <Field label={t('settings.translateBaseUrl')} hint={t('settings.translateBaseUrlHint')}>
          <input className="input" value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} placeholder="https://api.deepseek.com/v1" />
        </Field>
        <Field label={t('settings.translateModel')} hint={t('settings.translateModelHint')}>
          <input className="input" value={model} onChange={(e) => setModel(e.target.value)} placeholder="deepseek-flash" />
        </Field>
        <Field label={t('settings.translateKey')} hint={config?.has_key ? t('settings.translateKeySaved') : t('settings.translateKeyPlaceholder')}>
          <div className="tr-key-row">
            <input
              type="password"
              autoComplete="off"
              className="input"
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              placeholder={config?.has_key ? '••••••••' : 'sk-...'}
            />
            {config?.has_key && (
              <button type="button" className="btn" disabled={saving} onClick={() => void clearKey()}>
                {t('settings.translateClearKey')}
              </button>
            )}
          </div>
        </Field>

        <div className="tr-preview">
          <div className="tr-preview-meta">
            <span className="tr-preview-label">{t('settings.translatePreview')}</span>
            {testing ? (
              <span className="tr-preview-status">{t('convo.translate.loading')}</span>
            ) : testState?.ok ? (
              <span className="tr-preview-status is-ok">{t('settings.translateTestOk', { ms: testState.latency ?? 0 })}</span>
            ) : testState && !testState.ok ? (
              <span className="tr-preview-status is-err">{testState.error}</span>
            ) : (
              <span className="tr-preview-status">{t('settings.translatePreviewIdle')}</span>
            )}
          </div>
          <div className="tr-preview-card">
            <button
              type="button"
              className="tr-preview-head"
              aria-expanded={previewOpen}
              onClick={() => setPreviewOpen((v) => !v)}
            >
              <Brain size={14} className="tr-preview-ic" aria-hidden="true" />
              <span className="tr-preview-title">{previewOpen ? t('convo.thinking') : headTitle}</span>
              {previewOpen ? <ChevronDown size={14} className="tr-preview-chev" aria-hidden="true" /> : <ChevronRight size={14} className="tr-preview-chev" aria-hidden="true" />}
            </button>
            {previewOpen && (
              <div className="tr-preview-body">
                {mode === 'summarize' ? (
                  <>
                    <Markdown text={originalText} />
                    {(translatedText || testing) && (
                      <div className="tr-preview-sum">
                        <div className="tr-preview-sum-head">{t('convo.translate.summaryLabel')}</div>
                        {testing ? (
                          <span className="tr-preview-status">{t('convo.translate.summarizing')}</span>
                        ) : (
                          translatedText && <Markdown text={translatedText} />
                        )}
                      </div>
                    )}
                  </>
                ) : (
                  <>
                    {(translatedText || testing) && (
                      <div className="tr-preview-bar">
                        {testing && <span className="tr-preview-status">{t('convo.translate.loading')}</span>}
                        {translatedText && (
                          <button
                            type="button"
                            className="tr-preview-toggle"
                            aria-pressed={showOrig}
                            onClick={() => setShowOrig((v) => !v)}
                          >
                            {t(showOrig ? 'convo.translate.translated' : 'convo.translate.original')}
                          </button>
                        )}
                      </div>
                    )}
                    <Markdown text={shownText} />
                  </>
                )}
              </div>
            )}
            <p className="tr-preview-reply">{t('settings.translatePreviewReply')}</p>
          </div>
        </div>

        <div className="dialog-actions">
          <button type="button" className="btn" disabled={testing || saving} onClick={() => void runTest()}>
            {testing ? t('settings.translateTesting') : t('settings.translateTest')}
          </button>
          <span className="tr-actions-spacer" />
          <button type="button" className="btn" disabled={saving} onClick={onClose}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={saving || !dirty}>
            {t('common.save')}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

// The display name, edited in place; Save shows once it differs.
function ProfileSetting({ onDirty }: { onDirty: (dirty: boolean) => void }) {
  const { me, setMe } = useSession()
  const toast = useToast()
  const [display, setDisplay] = useState(me?.display_name ?? '')
  const [busy, setBusy] = useState(false)
  const dirty = display !== (me?.display_name ?? '')
  useEffect(() => onDirty(dirty), [dirty, onDirty])
  const save = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || !dirty) return
    setBusy(true)
    try {
      const m = await api<Me>('PUT', '/api/me', { display_name: display })
      setMe(m)
      toast(t('settings.saved'))
    } catch (err) {
      toast(errorText(err), true)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="setting setting-wide" onSubmit={save}>
      <div className="setting-label">
        <label htmlFor="s-display">{t('settings.displayName')}</label>
        <span className="hint" id="s-display-hint">{t('settings.username', { name: me?.username ?? '' })}</span>
      </div>
      <div className="setting-control">
        <input id="s-display" className="input" aria-describedby="s-display-hint" value={display} onChange={(e) => setDisplay(e.target.value)} />
        {dirty && <button className="btn" disabled={busy}>{t('common.save')}</button>}
      </div>
    </form>
  )
}

function PasswordSetting() {
  const [open, setOpen] = useState(false)
  const { me } = useSession()
  // (Single sign-on its only way in: no password to change — an administrator gives it one.)
  const none = me?.has_password === false
  return (
    <Setting label={t('settings.passwordRow')} hint={none ? t('sso.noPassword') : undefined}>
      <button type="button" className="btn" onClick={() => setOpen(true)} disabled={none}>{t('settings.password')}</button>
      {open && <PasswordDialog onClose={() => setOpen(false)} />}
    </Setting>
  )
}

// The account's single sign-on: the identity it is linked to (and undoing
// that, where a password is left to sign in with), or linking one — off to
// the provider and back here.
function SSOSetting() {
  const toast = useToast()
  const { boot } = useSession()
  const [me, setMe] = useState<Me | null>(null)
  const [busy, setBusy] = useState(false)
  const [asking, setAsking] = useState(false)
  const [err, setErr] = useState(() => new URLSearchParams(location.search).get('sso_error') ?? '')
  // (Not read: said so, with a retry — never shown as "not linked".)
  const [failed, setFailed] = useState(false)
  const load = () => {
    setFailed(false)
    return api<Me>('GET', '/api/me').then(setMe, () => setFailed(true))
  }
  useEffect(() => {
    void load()
    const q = new URLSearchParams(location.search)
    if (q.get('sso') === 'linked') toast(t('sso.linked'))
    // (Said once: not again on a reload.)
    if (q.has('sso') || q.has('sso_error')) history.replaceState(history.state, '', location.pathname)
  }, [toast])
  const id = me?.sso?.[0]
  const link = async () => {
    if (busy) return
    setBusy(true)
    setErr('')
    try {
      const r = await api<{ url: string }>('POST', '/api/me/oidc/link')
      location.assign(r.url)
    } catch (e) {
      // (The provider's state in the page's words: sso.error.*, as the sign-in page says it.)
      toast(e instanceof ApiError && hasKey('sso.error.' + e.code) ? ssoErrorText(e.code) : errorText(e), true)
      setBusy(false)
    }
  }
  const unlink = async () => {
    await api('DELETE', '/api/me/oidc')
    toast(t('sso.unlinked'))
    await load()
  }
  // (Its password one it may sign in with: else single sign-on is its only way in, and it stays.)
  const pwOK = !!me?.has_password && (boot?.local_login === 'all' || !boot?.local_login || (boot.local_login === 'admins' && me?.role === 'admin'))
  const who = id ? id.username_hint || id.email || id.subject : ''
  const host = id ? (() => { try { return new URL(id.issuer).host } catch { return id.issuer } })() : ''
  if (failed) {
    return (
      <Setting label={t('sso.row')}>
        <LoadError text={t('sso.loadFailed')} onRetry={() => void load()} />
      </Setting>
    )
  }
  return (
    <Setting
      label={t('sso.row')}
      hint={
        <>
          {!me ? t('common.loading') : id ? t('sso.linkedAs', { who, host }) : t('sso.notLinked')}
          {id && !pwOK && (
            <span className="hint-block">
              {t(boot?.local_login === 'off' ? 'sso.onlyWayOff' : me?.has_password ? 'sso.onlyWayMode' : 'sso.onlyWay')}
            </span>
          )}
          {err && <span className="err hint-block" role="alert">{ssoErrorText(err)}</span>}
        </>
      }
    >
      {id ? (
        <button type="button" className="btn" onClick={() => setAsking(true)} disabled={!pwOK}>
          {t('sso.unlink')}
        </button>
      ) : (
        <button type="button" className="btn" onClick={link} disabled={busy || !me}>
          {t('sso.link')}
        </button>
      )}
      {asking && <Confirm title={t('sso.unlinkTitle')} body={t('sso.unlinkBody')} confirm={t('sso.unlink')} danger onConfirm={unlink} onClose={() => setAsking(false)} />}
    </Setting>
  )
}

function PasswordDialog({ onClose }: { onClose: () => void }) {
  const toast = useToast()
  const [cur, setCur] = useState('')
  const [next, setNext] = useState('')
  // The new one twice, as when a password must be changed (ux1101 PG-51).
  const [again, setAgain] = useState('')
  const [mismatch, setMismatch] = useState(false)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const close = () => !busy && onClose()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    if (next !== again) {
      setError('') // (not the last try's error beside this one — review)
      setMismatch(true)
      document.getElementById('s-again')?.focus()
      return
    }
    setMismatch(false)
    setBusy(true)
    setError('')
    try {
      await api('POST', '/api/me/password', { current: cur, next })
      toast(t('settings.passwordChanged'))
      setBusy(false)
      onClose()
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return (
    <Dialog title={t('settings.password')} onClose={close} busy={busy} guard={!!(cur || next || again)} icon={<KeyRound size={18} />}>
      <form className="form" onSubmit={submit}>
        <Field label={t('settings.currentPassword')} htmlFor="s-cur">
          <input id="s-cur" className="input" type="password" autoComplete="current-password" value={cur} onChange={(e) => setCur(e.target.value)} required />
        </Field>
        <Field label={t('settings.newPassword')} htmlFor="s-next" hint={t('setup.passwordHint')}>
          <input id="s-next" className="input" type="password" autoComplete="new-password" minLength={8} value={next} onChange={(e) => setNext(e.target.value)} required />
        </Field>
        <Field label={t('settings.againPassword')} htmlFor="s-again" error={mismatch ? t('settings.passwordMismatch') : undefined}>
          <input id="s-again" className="input" type="password" autoComplete="new-password" value={again} onChange={(e) => (setAgain(e.target.value), setMismatch(false))} required aria-invalid={mismatch} />
        </Field>
        {error && <p className="err" role="alert">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={close} disabled={busy}>{t('common.cancel')}</button>
          <button className="btn btn-primary" disabled={busy || !cur || next.length < 8 || !again}>{t('settings.password')}</button>
        </div>
      </form>
    </Dialog>
  )
}

type PushInfo = { enabled: boolean; public_key?: string; devices?: { id: number; user_agent: string; created_at: number }[] }

// What the notification section shows: a server-side reason it cannot work,
// a browser state, or "registered" only once the server has confirmed it.
type PushView = PushState | 'loading' | 'load-failed' | 'unregistered'

function PushSection() {
  const toast = useToast()
  const [info, setInfo] = useState<PushInfo | null>(null)
  const [view, setView] = useState<PushView>('loading')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const refresh = async () => {
    setError('')
    let i: PushInfo
    try {
      i = await api<PushInfo>('GET', '/api/push')
    } catch {
      setView('load-failed')
      return
    }
    setInfo(i)
    if (!i.enabled) return setView('off')
    const st = await pushState().catch((): PushState => 'unsupported')
    // A subscription that exists in the browser but not on the server for
    // this account would never deliver, so confirm it before saying "on".
    if (st === 'on' && !(await syncPush())) return setView('unregistered')
    setView(st)
  }
  useEffect(() => {
    void refresh()
  }, [])

  const run = async (fn: () => Promise<unknown>, failText: string) => {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await fn()
    } catch (e) {
      setError(failText + (e instanceof Error && e.message ? ' ' + errorText(e) : ''))
    } finally {
      setBusy(false)
    }
  }

  let body: ReactNode = null
  if (view === 'loading') body = <p className="loading">{t('common.loading')}</p>
  else if (view === 'load-failed')
    body = (
      <p className="inline-error" role="alert">
        <span>{t('common.loadFailed')}</span>
        <button className="link-btn" onClick={refresh}>{t('common.retry')}</button>
      </p>
    )
  else if (info && !info.enabled) body = <p className="hint">{t('push.unavailable')}</p>
  else if (view === 'needs-home-screen') body = <p className="hint">{t('push.needsHomeScreen')}</p>
  else if (view === 'unsupported') body = <p className="hint">{t('push.unsupported')}</p>
  // Blocked in the browser: said plainly, with a way to look again once
  // allowed there (not as an error of ours — ux1101 PG-22).
  else if (view === 'denied')
    body = (
      <p className="hint push-denied">
        <span>{t('push.denied')}</span>{' '}
        <button type="button" className="link-btn" onClick={() => void refresh()}>{t('push.recheck')}</button>
      </p>
    )
  else {
    const on = view === 'on'
    body = (
      <div className="settings-group">
        <label className="setting">
          <span className="setting-label">
            <span>{t('push.thisDevice')}</span>
            <span className={view === 'unregistered' ? 'hint state-danger' : 'hint'}>
              {view === 'unregistered' ? t('push.unregistered') : on ? t('push.enabled') : t('push.offHint')}
            </span>
          </span>
          <span className="setting-control">
            <input
              type="checkbox"
              role="switch"
              className="switch"
              checked={on}
              disabled={busy}
              onChange={(e) =>
                run(async () => {
                  if (e.target.checked) await enablePush(info!.public_key!)
                  else await disablePush()
                  await refresh()
                }, e.target.checked ? t('push.failed') : t('push.disableFailed'))
              }
            />
          </span>
        </label>
        {on && (
          <Setting label={t('push.test')} hint={t('push.testHint')}>
            <button
              className="btn"
              disabled={busy}
              onClick={() =>
                run(async () => {
                  await api('POST', '/api/push/test')
                  toast(t('push.testSent'))
                }, t('push.testFailed'))
              }
            >
              {t('push.testSend')}
            </button>
          </Setting>
        )}
      </div>
    )
  }
  return (
    <section className="section section-ruled">
      <h2>{t('push.title')}</h2>
      <p className="hint">{t('push.intro')}</p>
      {body}
      {error && <p className="err" role="alert">{error}</p>}
    </section>
  )
}
