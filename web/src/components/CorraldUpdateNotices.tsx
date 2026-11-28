import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError } from '../api'
import {
  CORRALD_NOTICES_EVENT,
  getCorraldNotices,
  markCorraldNoticesSeen,
} from '../corraldApi'
import { groupCorraldUpdateNotices } from '../corraldLogic'
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
  const shownIdsRef = useRef<Set<number>>(new Set())
  const inFlightRef = useRef(false)

  useEffect(() => {
    shownIdsRef.current.clear()
    setChangelogVersion(null)
  }, [userId])

  const checkNotices = useCallback(
    async (signal?: AbortSignal) => {
      if (!userId || inFlightRef.current) return
      inFlightRef.current = true
      try {
        const raw = await getCorraldNotices(signal)
        const fresh = raw.filter((n) => !shownIdsRef.current.has(n.id))
        if (fresh.length === 0) return

        const grouped = groupCorraldUpdateNotices(fresh)
        if (grouped.allIds.length === 0) return

        for (const id of grouped.allIds) {
          shownIdsRef.current.add(id)
        }
        // Mark seen immediately so the same notices are never shown twice.
        void markCorraldNoticesSeen(grouped.allIds).catch(() => {})

        if (grouped.mergedSuccess) {
          const ver = grouped.mergedSuccess.version
          toast.notice({
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
          toast.notice({
            title: t('corrald.note.updateFailedTitle', { host: f.host_name || `#${f.host_id}` }),
            body: t('corrald.err.updateFailed', {
              next: f.new_version || '?',
              prev: f.old_version || '?',
            }),
            error: true,
          })
        }
      } catch (e) {
        if (e instanceof ApiError && (e.code === 'aborted' || e.status === 404)) return
      } finally {
        inFlightRef.current = false
      }
    },
    [userId, toast],
  )

  useEffect(() => {
    if (!userId) return
    const ctl = new AbortController()
    void checkNotices(ctl.signal)

    const onRefresh = () => void checkNotices()
    const onVis = () => {
      if (document.visibilityState === 'visible') {
        void checkNotices()
      }
    }
    window.addEventListener(CORRALD_NOTICES_EVENT, onRefresh)
    document.addEventListener('visibilitychange', onVis)
    return () => {
      ctl.abort()
      window.removeEventListener(CORRALD_NOTICES_EVENT, onRefresh)
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [userId, checkNotices])

  if (changelogVersion === null) return null
  return (
    <CorraldChangelogDialog
      currentVersion={changelogVersion}
      onClose={() => setChangelogVersion(null)}
    />
  )
}
