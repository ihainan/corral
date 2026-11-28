import { useEffect, useRef, useState } from 'react'
import { ApiError } from '../api'
import {
  CORRALD_NOTICES_EVENT,
  getCorraldNotices,
  markCorraldNoticesSeen,
} from '../corraldApi'
import {
  createCorraldNoticeFetcher,
  type CorraldNoticeFetcher,
  type GroupedCorraldUpdateNotices,
} from '../corraldLogic'
import { t } from '../i18n'
import { CorraldChangelogDialog } from './CorraldDialogs'
import { useToast } from './ui'

// Fetches unseen corrald update notices when a user opens Corral (or when a
// live update finishes), merges all successful host updates into a single
// corner notice with a "更新内容" button, emits one notice per failed host,
// and marks all displayed notices as seen immediately.
export function CorraldUpdateNotices({ userId }: { userId?: number }) {
  const toast = useToast()
  const [changelogVersion, setChangelogVersion] = useState<string | null>(null)
  const toastRef = useRef(toast)
  toastRef.current = toast
  const fetcherRef = useRef<CorraldNoticeFetcher | null>(null)

  if (!fetcherRef.current) {
    fetcherRef.current = createCorraldNoticeFetcher({
      fetchNotices: (signal) => getCorraldNotices(signal),
      markSeen: (ids) => markCorraldNoticesSeen(ids),
      onNotices: (grouped: GroupedCorraldUpdateNotices) => {
        if (grouped.mergedSuccess) {
          const ver = grouped.mergedSuccess.version
          toastRef.current.notice({
            // Builds are named like v0.2.0-workbench-<date>-<commit>; the toast
            // names the release (0.2.0), the host settings show the full build.
            title: t('corrald.note.updatedTitle', { version: ver.replace(/^v/, '').split('-')[0] || ver }),
            body: t('corrald.note.updatedHosts', { n: grouped.mergedSuccess.hostCount }),
            action: {
              label: t('corrald.changelogLink'),
              run: () => setChangelogVersion(ver),
            },
          })
        }

        for (const f of grouped.failures) {
          toastRef.current.notice({
            title: t('corrald.note.updateFailedTitle', { host: f.host_name || `#${f.host_id}` }),
            body: t('corrald.err.updateFailed', {
              next: f.new_version || '?',
              prev: f.old_version || '?',
            }),
            error: true,
          })
        }
      },
      onError: (e) => {
        if (e instanceof ApiError && (e.code === 'aborted' || e.status === 404)) return
      },
    })
  }

  useEffect(() => {
    const fetcher = fetcherRef.current!
    fetcher.setUser(userId)
    setChangelogVersion(null)
    if (!userId) return

    void fetcher.check()

    const onRefresh = () => void fetcher.check()
    const onVis = () => {
      if (document.visibilityState === 'visible') {
        void fetcher.check()
      }
    }
    window.addEventListener(CORRALD_NOTICES_EVENT, onRefresh)
    document.addEventListener('visibilitychange', onVis)
    return () => {
      fetcher.dispose()
      window.removeEventListener(CORRALD_NOTICES_EVENT, onRefresh)
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [userId])

  if (changelogVersion === null) return null
  return (
    <CorraldChangelogDialog
      currentVersion={changelogVersion}
      onClose={() => setChangelogVersion(null)}
    />
  )
}
