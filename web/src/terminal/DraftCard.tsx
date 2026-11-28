import { t } from '../i18n'

// Native card shown above the composer when the agent's terminal input box
// holds recognizable unsent text, replacing the raw terminal popup.
// Sending and clearing both go through POST /conversation/box-draft, which
// verifies the screen digest before pressing Enter or Ctrl+C.
export function DraftCard({
  draft,
  busy,
  error,
  onSend,
  onClear,
  onTerminal,
  onClose,
}: {
  draft: string
  busy: '' | 'send' | 'clear'
  error?: string
  onSend: () => void
  onClear: () => void
  onTerminal: () => void
  onClose: () => void
}) {
  const disabled = busy !== ''
  return (
    <div className="convo-draft-card" role="group" aria-label={t('draftCard.tag')}>
      <div className="convo-draft-card-head">
        <span className="convo-draft-card-tag">{t('draftCard.tag')}</span>
        <div className="convo-draft-card-links">
          <button type="button" className="link-btn convo-draft-card-link" disabled={disabled} onClick={onTerminal}>
            {t('draftCard.inTerminal')}
          </button>
          <button type="button" className="link-btn convo-draft-card-link" disabled={disabled} onClick={onClose}>
            {t('common.close')}
          </button>
        </div>
      </div>
      <p className="convo-draft-card-lead">{t('draftCard.lead')}</p>
      <p className="convo-draft-card-quote">{draft}</p>
      <div className="convo-draft-card-acts">
        <button type="button" className="btn btn-primary convo-draft-card-act" disabled={disabled} onClick={onSend}>
          {busy === 'send' ? t('draftCard.sending') : t('draftCard.send')}
        </button>
        <button type="button" className="btn btn-ghost convo-draft-card-act" disabled={disabled} onClick={onClear}>
          {busy === 'clear' ? t('draftCard.clearing') : t('draftCard.clear')}
        </button>
      </div>
      {error && (
        <p className="convo-draft-card-stat is-bad" role="status">
          {error}
        </p>
      )}
    </div>
  )
}
