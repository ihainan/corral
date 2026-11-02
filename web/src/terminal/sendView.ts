import { t } from '../i18n'

// What the gateway says when Claude Code's input box is not this
// conversation's (a subagent's shown in front, a list holding the keys, no
// box seen): nothing typed, or typed and no Enter — plans/send-view.md.
// null for any other refusal.
export function viewRefusal(code: string | undefined): string | null {
  switch (code) {
    case 'subagent_view':
      return t('convo.err.subagentView')
    case 'other_view':
      return t('convo.err.otherView')
    case 'no_input':
      return t('convo.err.noInput')
    case 'typed_other_view':
      return t('convo.err.typedOtherView')
  }
  return null
}
