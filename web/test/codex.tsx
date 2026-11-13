// Browser fixture only: Vite's production entry remains index.html.
import { useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { setLang } from '../src/i18n'
import { ConversationView } from '../src/terminal/Conversation'
import { useConversation } from '../src/terminal/conversation'
import { PictureDialog } from '../src/terminal/picture'
import '../src/styles/app.css'
import '../src/styles/terminal.css'

const query = new URLSearchParams(location.search)
setLang(query.get('lang') === 'zh' ? 'zh-CN' : 'en')
document.documentElement.classList.add('term-mode')
const dark = query.get('theme') === 'dark'
document.documentElement.classList.toggle('dark', dark)

function Fixture() {
  const convo = useConversation('fixture', true, 'p1')
  const [picture, setPicture] = useState<string | null>(null)
  useEffect(() => {
    const refresh = () => convo.refresh()
    window.addEventListener('fixture-refresh', refresh)
    return () => window.removeEventListener('fixture-refresh', refresh)
  }, [convo.refresh])
  return (
    <div className={'term-root' + (dark ? ' dark' : '')}>
      <ConversationView
        convo={convo}
        agent={{ title: 'Codex fixture', program: 'codex', status: 'idle' }}
        pending={[]}
        onDismiss={() => {}}
        onShowTerminal={() => {}}
        onCopied={() => {}}
        files={{ view: setPicture, reveal: () => {}, download: () => {} }}
      />
      {picture && <PictureDialog sid="fixture" path={picture} onClose={() => setPicture(null)} />}
    </div>
  )
}

createRoot(document.getElementById('root')!).render(<Fixture />)
