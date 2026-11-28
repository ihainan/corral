import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/corraldLogic.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  DEFAULT_UNINSTALL_CMD,
  buildDiagnostics,
  buildRemoveFilesCommand,
  buildStopCommand,
  classifyDeleteHostResult,
  corraldFailureKind,
  corraldHintMessageKey,
  corraldSectionModel,
  corraldStatusBarModel,
  createCorraldHintSync,
  createCorraldNoticeFetcher,
  degradedCaps,
  detectCorraldTransition,
  formatPerfSeconds,
  formatRssMb,
  groupCorraldUpdateNotices,
  hasInstalledCorrald,
  isCorraldStatusHintEligible,
  isCorraldUnknownResult,
  isOpActive,
  isOpCancellable,
  opProgress,
  otherUsersCount,
  shouldPollCorrald,
  shouldShowCorraldHint,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

function baseStatus(overrides = {}) {
  return {
    state: 'off',
    running: null,
    users: null,
    consent: null,
    op: null,
    caps: null,
    scope_change: null,
    error: null,
    admin_disabled: false,
    files: [],
    ...overrides,
  }
}

// 1. Every spec §7 state produces the expected section model and actions.
{
  const mOff = corraldSectionModel(baseStatus({ state: 'off' }))
  assert.equal(mOff.mode, 'off')
  assert.equal(mOff.poll, false)
  assert.deepEqual(mOff.actions, ['enable'])
  assert.equal(mOff.showDetailsRows, false)
  assert.equal(mOff.showReadmeLink, true)
  assert.equal(hasInstalledCorrald(baseStatus({ state: 'off' })), false)

  // installing: cancellable before start, not cancellable at/after start
  const stInstallUpload = baseStatus({
    state: 'installing',
    op: { id: 'a1', kind: 'install', state: 'running', step: 'upload', steps: ['check', 'upload', 'verify', 'install', 'start'] },
  })
  const mInstallUpload = corraldSectionModel(stInstallUpload)
  assert.equal(mInstallUpload.mode, 'installing')
  assert.equal(mInstallUpload.poll, true)
  assert.deepEqual(mInstallUpload.actions, ['cancel'])
  assert.equal(mInstallUpload.progress.current, 2)
  assert.equal(mInstallUpload.progress.total, 5)
  assert.equal(mInstallUpload.progress.cancellable, true)

  const stInstallStart = baseStatus({
    state: 'installing',
    op: { id: 'a1', kind: 'install', state: 'running', step: 'start', steps: ['check', 'upload', 'verify', 'install', 'start'] },
  })
  const mInstallStart = corraldSectionModel(stInstallStart)
  assert.equal(mInstallStart.mode, 'installing')
  assert.equal(mInstallStart.progress.cancellable, false)
  assert.deepEqual(mInstallStart.actions, [])

  // running: grouped details (version, rss, uptime, users, files), pause + remove
  const stRunning = baseStatus({
    state: 'running',
    version: '0.3.1',
    running: { pid: 1234, uptime_s: 3660, rss_kb: 28672 },
    users: { count: 2, others: ['bob'] },
    caps: { locate: 'ok', watch: 'ok', convo: 'ok' },
    files: ['~/.local/lib/corral/corrald-0.3.1-100/corrald'],
  })
  const mRunning = corraldSectionModel(stRunning)
  assert.equal(mRunning.mode, 'running')
  assert.equal(mRunning.poll, false)
  assert.equal(mRunning.showDetailsRows, true)
  assert.equal(mRunning.showVersionRow, true)
  assert.equal(mRunning.showUsersRow, true)
  assert.equal(mRunning.showFiles, true)
  assert.equal(mRunning.rssMb, 28)
  assert.equal(mRunning.uptimeSec, 3660)
  assert.equal(mRunning.otherUsers, 1)
  assert.deepEqual(mRunning.degraded, [])
  assert.deepEqual(mRunning.actions, ['pause', 'remove'])
  assert.equal(hasInstalledCorrald(stRunning), true)

  // running with degraded caps
  const mDegraded = corraldSectionModel(
    baseStatus({
      state: 'running',
      version: '0.3.1',
      caps: { locate: 'ok', watch: 'failed', convo: 'degraded' },
    }),
  )
  assert.deepEqual(mDegraded.degraded, ['convo', 'watch'])

  // stopped (normal)
  const mStopped = corraldSectionModel(baseStatus({ state: 'stopped', version: '0.3.1', users: { count: 1, others: [] } }))
  assert.equal(mStopped.mode, 'stopped')
  assert.equal(mStopped.poll, false)
  assert.equal(mStopped.showVersionRow, true)
  assert.deepEqual(mStopped.actions, ['remove'])

  // updating
  const mUpdating = corraldSectionModel(
    baseStatus({
      state: 'updating',
      version: '0.3.1',
      gateway_version: '0.3.2',
      op: { id: 'u1', kind: 'update', state: 'running', step: 'handoff', steps: ['upload', 'install', 'handoff', 'verify'] },
    }),
  )
  assert.equal(mUpdating.mode, 'updating')
  assert.equal(mUpdating.poll, true)
  assert.equal(mUpdating.progress.current, 3)
  assert.equal(mUpdating.progress.total, 4)
  assert.deepEqual(mUpdating.actions, [])

  // paused
  const mPaused = corraldSectionModel(baseStatus({ state: 'paused', version: '0.3.1' }))
  assert.equal(mPaused.mode, 'paused')
  assert.equal(mPaused.poll, false)
  assert.deepEqual(mPaused.actions, ['resume', 'remove'])

  // lost
  const mLost = corraldSectionModel(baseStatus({ state: 'lost', version: '0.3.1', last_ok_at: 1700000000 }))
  assert.equal(mLost.mode, 'lost')
  assert.equal(mLost.poll, false)
  assert.deepEqual(mLost.actions, ['remove'])

  // error
  const mError = corraldSectionModel(
    baseStatus({
      state: 'error',
      error: { code: 'start_failed', detail: 'exit 1', os: 'linux', arch: 'amd64' },
      machine_id: 'abc123def456',
    }),
  )
  assert.equal(mError.mode, 'error')
  assert.equal(mError.poll, false)
  assert.equal(mError.failure.kind, 'start_failed')
  assert.equal(mError.failure.canCopyDiag, true)
  assert.deepEqual(mError.actions, ['retry', 'remove'])

  // removing
  const mRemoving = corraldSectionModel(
    baseStatus({
      state: 'removing',
      op: { id: 'r1', kind: 'remove', state: 'running', step: 'stop' },
    }),
  )
  assert.equal(mRemoving.mode, 'removing')
  assert.equal(mRemoving.poll, true)
  assert.deepEqual(mRemoving.actions, [])

  // remove_pending
  const mRemovePending = corraldSectionModel(baseStatus({ state: 'remove_pending' }))
  assert.equal(mRemovePending.mode, 'remove_pending')
  assert.equal(mRemovePending.poll, false)
  assert.equal(mRemovePending.removeOutcome.kind, 'remove_pending')
  assert.equal(mRemovePending.removeOutcome.cmd, DEFAULT_UNINSTALL_CMD)
  assert.deepEqual(mRemovePending.actions, [])

  // rebind (host identity changed)
  const mRebind = corraldSectionModel(baseStatus({ state: 'rebind', version: '0.3.1' }))
  assert.equal(mRebind.mode, 'rebind')
  assert.equal(mRebind.poll, false)
  assert.deepEqual(mRebind.actions, ['rebind', 'remove'])

  // incompatible (gateway older than host daemon)
  const mIncompat = corraldSectionModel(baseStatus({ state: 'incompatible', version: '0.4.0' }))
  assert.equal(mIncompat.mode, 'incompatible')
  assert.equal(mIncompat.poll, false)
  assert.equal(mIncompat.showVersionRow, true)
  assert.deepEqual(mIncompat.actions, ['replace_gateway', 'remove'])
}

// 2. Admin disabled overrides normal controls and suppresses scope change banner.
{
  const stAdmin = baseStatus({
    state: 'running',
    version: '0.3.1',
    admin_disabled: true,
    scope_change: { target_version: '0.4.0', added: ['agent-logs:opencode'] },
  })
  const m = corraldSectionModel(stAdmin)
  assert.equal(m.mode, 'admin_disabled')
  assert.deepEqual(m.actions, [])
  assert.equal(m.scopeChange, null)
  assert.equal(m.poll, false)
  assert.equal(corraldStatusBarModel(stAdmin), null)

  // Admin disabled with an active removal op still polls until removal completes.
  const stAdminRemoving = baseStatus({
    state: 'removing',
    admin_disabled: true,
    op: { id: 'r2', kind: 'remove', state: 'running', step: 'remove' },
  })
  assert.equal(shouldPollCorrald(stAdminRemoving), true)
}

// 3. Scope change banner visibility & "暂不更新" dismissal.
{
  const stScope = baseStatus({
    state: 'running',
    version: '0.3.2',
    scope_change: { target_version: '0.4.0', added: ['agent-logs:opencode'] },
  })
  const m1 = corraldSectionModel(stScope)
  assert.deepEqual(m1.scopeChange, {
    targetVersion: '0.4.0',
    added: ['agent-logs:opencode'],
    summary: undefined,
  })
  const m2 = corraldSectionModel(stScope, { dismissedScopeVersion: '0.4.0' })
  assert.equal(m2.scopeChange, null)
  // A newer version re-prompts even if 0.4.0 was dismissed.
  const m3 = corraldSectionModel(
    baseStatus({
      state: 'running',
      version: '0.3.2',
      scope_change: { target_version: '0.5.0', added: ['actions:mode'] },
    }),
    { dismissedScopeVersion: '0.4.0' },
  )
  assert.equal(m3.scopeChange?.targetVersion, '0.5.0')
}

// 4. Operation outcomes & failure mapping.
{
  assert.equal(corraldFailureKind('unsupported_os'), 'unsupported_os')
  assert.equal(corraldFailureKind('unsupported_arch'), 'unsupported_arch')
  assert.equal(corraldFailureKind('home_readonly'), 'home_readonly')
  assert.equal(corraldFailureKind('no_space'), 'no_space')
  assert.equal(corraldFailureKind('checksum_mismatch'), 'checksum_mismatch')
  assert.equal(corraldFailureKind('start_failed'), 'start_failed')
  assert.equal(corraldFailureKind('disconnected'), 'disconnected')
  assert.equal(corraldFailureKind('update_failed'), 'update_failed')
  assert.equal(corraldFailureKind('handoff_failed'), 'handoff_failed')
  assert.equal(corraldFailureKind('handoff_in_progress'), 'handoff_in_progress')
  assert.equal(corraldFailureKind('not_newer'), 'not_newer')
  assert.equal(corraldFailureKind('proto_mismatch'), 'proto_mismatch')
  assert.equal(corraldFailureKind('op_expired'), 'op_unknown')
  assert.equal(corraldFailureKind('op_unknown'), 'op_unknown')
  assert.equal(corraldFailureKind('corrald_unknown_result'), 'op_unknown')
  assert.equal(corraldFailureKind('stop_failed'), 'stop_failed')
  assert.equal(corraldFailureKind('partial'), 'partial')
  assert.equal(corraldFailureKind('removing'), 'removing')
  assert.equal(corraldFailureKind('unknown_code'), 'other')

  // Partial remove in section model
  const stPartial = baseStatus({
    state: 'error',
    op: {
      id: 'r3',
      kind: 'remove',
      state: 'failed',
      step: 'verify',
      result: 'partial',
      left: [{ path: '/home/dev/.local/lib/corral/corrald', reason: 'permission denied' }],
    },
  })
  const mPartial = corraldSectionModel(stPartial)
  assert.equal(mPartial.removeOutcome.kind, 'partial')
  assert.equal(mPartial.removeOutcome.cmd, 'rm -rf -- /home/dev/.local/lib/corral/corrald')

  // Shell quoting for paths containing spaces/quotes
  assert.equal(
    buildRemoveFilesCommand([{ path: "/tmp/my dir/a'b" }]),
    `rm -rf -- '/tmp/my dir/a'\\''b'`,
  )

  // Stop failed in section model
  const stStopFailed = baseStatus({
    state: 'error',
    op: { id: 'r4', kind: 'remove', state: 'failed', step: 'stop', result: 'stop_failed', pid: 4321 },
  })
  const mStop = corraldSectionModel(stStopFailed)
  assert.equal(mStop.removeOutcome.kind, 'stop_failed')
  assert.equal(mStop.removeOutcome.pid, 4321)
  assert.equal(mStop.removeOutcome.cmd, 'kill 4321')
  assert.equal(buildStopCommand(undefined), DEFAULT_UNINSTALL_CMD)

  // Rollback after update_failed while still running on previous version
  const stUpdateFail = baseStatus({
    state: 'running',
    version: '0.3.1',
    error: { code: 'update_failed', target_version: '0.3.2', fallback_version: '0.3.1' },
  })
  const mUF = corraldSectionModel(stUpdateFail)
  assert.deepEqual(mUF.updateFailure, { targetVersion: '0.3.2', fallbackVersion: '0.3.1' })
}

// 5. Transitions for corner notifications and outcomes.
{
  const installing = baseStatus({
    state: 'installing',
    op: { id: 'i1', kind: 'install', state: 'running', step: 'upload' },
  })
  const enabled = baseStatus({ state: 'running', version: '0.3.1', agents_count: 12 })
  assert.deepEqual(detectCorraldTransition(installing, enabled), {
    type: 'enabled',
    version: '0.3.1',
    agentsCount: 12,
  })

  const installFailed = baseStatus({
    state: 'off',
    op: { id: 'i1', kind: 'install', state: 'failed', step: 'verify', error_code: 'checksum_mismatch' },
  })
  assert.deepEqual(detectCorraldTransition(installing, installFailed), {
    type: 'install_failed',
    code: 'checksum_mismatch',
    detail: undefined,
  })

  const installCancelled = baseStatus({
    state: 'off',
    op: { id: 'i1', kind: 'install', state: 'cancelled', step: 'upload' },
  })
  assert.deepEqual(detectCorraldTransition(installing, installCancelled), {
    type: 'install_cancelled',
  })

  const removing = baseStatus({
    state: 'removing',
    op: { id: 'r1', kind: 'remove', state: 'running', step: 'stop' },
  })
  assert.deepEqual(
    detectCorraldTransition(removing, baseStatus({ state: 'off', op: { id: 'r1', kind: 'remove', state: 'done', step: 'verify', result: 'removed' } })),
    { type: 'removed' },
  )
  assert.deepEqual(
    detectCorraldTransition(
      removing,
      baseStatus({
        state: 'error',
        op: { id: 'r1', kind: 'remove', state: 'failed', step: 'verify', result: 'partial', left: [{ path: '/tmp/a', reason: 'EACCES' }] },
      }),
    ),
    { type: 'remove_partial', left: [{ path: '/tmp/a', reason: 'EACCES' }], cmd: 'rm -rf -- /tmp/a' },
  )
  assert.deepEqual(
    detectCorraldTransition(
      removing,
      baseStatus({ state: 'error', op: { id: 'r1', kind: 'remove', state: 'failed', step: 'stop', result: 'stop_failed', pid: 999 } }),
    ),
    { type: 'remove_stop_failed', pid: 999, cmd: 'kill 999' },
  )
  assert.deepEqual(
    detectCorraldTransition(removing, baseStatus({ state: 'remove_pending' })),
    { type: 'remove_pending', cmd: DEFAULT_UNINSTALL_CMD },
  )

  const updating = baseStatus({
    state: 'updating',
    version: '0.3.1',
    op: { id: 'u1', kind: 'update', state: 'running', step: 'handoff' },
  })
  assert.deepEqual(
    detectCorraldTransition(updating, baseStatus({ state: 'running', version: '0.3.2', op: { id: 'u1', kind: 'update', state: 'done', step: 'verify' } })),
    { type: 'updated', version: '0.3.2' },
  )
  // Update rolled back -> must NOT emit 'updated' notice
  assert.equal(
    detectCorraldTransition(
      updating,
      baseStatus({ state: 'running', version: '0.3.1', error: { code: 'update_failed' } }),
    ),
    null,
  )

  // Lost & error transitions
  assert.deepEqual(detectCorraldTransition(enabled, baseStatus({ state: 'lost' })), { type: 'lost' })
  assert.deepEqual(
    detectCorraldTransition(enabled, baseStatus({ state: 'error', error: { code: 'start_failed' } })),
    { type: 'error', code: 'start_failed' },
  )
}

// 6. Workbench status bar model.
{
  assert.deepEqual(corraldStatusBarModel(baseStatus({ state: 'running', version: '0.3.1' })), {
    kind: 'running',
    version: '0.3.1',
    degraded: false,
  })
  assert.deepEqual(corraldStatusBarModel(baseStatus({ state: 'updating', version: '0.3.1' })), {
    kind: 'updating',
    version: '0.3.1',
    degraded: false,
  })
  for (const hidden of ['off', 'installing', 'stopped', 'paused', 'lost', 'error', 'removing', 'remove_pending', 'rebind', 'incompatible']) {
    assert.equal(corraldStatusBarModel(baseStatus({ state: hidden, version: '0.3.1' })), null, `status bar should hide ${hidden}`)
  }
}

// 7. Delete host outcome classification.
{
  assert.deepEqual(classifyDeleteHostResult(null, true), { outcome: 'done' })
  assert.deepEqual(classifyDeleteHostResult({ reachable: false }, false), { outcome: 'done' })
  assert.deepEqual(classifyDeleteHostResult({ reachable: false }, true), {
    outcome: 'unreachable',
    cmd: DEFAULT_UNINSTALL_CMD,
  })
  assert.deepEqual(classifyDeleteHostResult({ corrald: { state: 'removed' } }, true), { outcome: 'removed' })
  assert.deepEqual(
    classifyDeleteHostResult({ corrald: { state: 'partial', left: [{ path: '/a/b', reason: 'ro' }] } }, true),
    { outcome: 'partial', left: [{ path: '/a/b', reason: 'ro' }], cmd: 'rm -rf -- /a/b' },
  )
  assert.deepEqual(classifyDeleteHostResult({ corrald: { state: 'stop_failed', pid: 77 } }, true), {
    outcome: 'stop_failed',
    pid: 77,
    cmd: 'kill 77',
  })
}

// 8. 409 corrald_unknown_result detection & helpers.
{
  assert.equal(isCorraldUnknownResult({ status: 409, code: 'corrald_unknown_result' }), true)
  assert.equal(isCorraldUnknownResult({ status: 500, code: 'corrald_unknown_result' }), false)
  assert.equal(isCorraldUnknownResult({ status: 409, code: 'screen_changed' }), false)
  assert.equal(isCorraldUnknownResult(null), false)

  assert.equal(isOpActive({ id: '1', kind: 'install', state: 'pending', step: 'check' }), true)
  assert.equal(isOpActive({ id: '1', kind: 'install', state: 'done', step: 'start' }), false)
  assert.equal(isOpCancellable({ id: '1', kind: 'remove', state: 'running', step: 'stop' }), false)
  assert.equal(opProgress({ id: '1', kind: 'install', state: 'done', step: 'start' }).percent, 100)
  assert.equal(formatRssMb(28672), 28)
  assert.equal(formatRssMb(0), null)
  assert.equal(otherUsersCount({ count: 3, others: ['b', 'c'] }), 2)
  assert.deepEqual(degradedCaps({ a: 'ok', b: 'error' }), ['b'])

  const diag = buildDiagnostics(
    baseStatus({
      state: 'error',
      version: '0.3.1',
      error: { code: 'start_failed', detail: 'bind failed', os: 'linux', arch: 'arm64' },
    }),
    'dev2',
  )
  assert.ok(diag.includes('host: dev2'))
  assert.ok(diag.includes('error: start_failed'))
  assert.ok(diag.includes('system: linux/arm64'))
}

// 9. Phase 2a: measured latency comparison thresholds, formatting, and update notice merging.
{
  assert.equal(formatPerfSeconds(300), '0.3')
  assert.equal(formatPerfSeconds(1900), '1.9')
  assert.equal(formatPerfSeconds(40), '0.04')
  assert.equal(formatPerfSeconds(0), '0.0')

  const mPerf = corraldSectionModel(
    baseStatus({
      state: 'running',
      version: '0.3.2',
      perf_comparison: [
        { op: 'send', after_ms: 300, before_ms: 1900, after_n: 25, before_n: 30 },
        { op: 'card', after_ms: 400, before_ms: 2000, after_n: 19, before_n: 25 }, // < 20 must be filtered out
      ],
    }),
  )
  assert.equal(mPerf.perfComparison.length, 1)
  assert.equal(mPerf.perfComparison[0].op, 'send')
  assert.equal(mPerf.showChangelogLink, true)

  const mAdminOff = corraldSectionModel(
    baseStatus({
      state: 'running',
      admin_disabled: true,
      perf_comparison: [{ op: 'send', after_ms: 300, before_ms: 1900, after_n: 30, before_n: 30 }],
    }),
  )
  assert.equal(mAdminOff.perfComparison.length, 0)
  assert.equal(mAdminOff.showChangelogLink, false)

  const grouped = groupCorraldUpdateNotices([
    { id: 1, host_id: 10, host_name: 'a', old_version: '0.3.0', new_version: '0.3.1', new_release: 1000, success: true, created_at: 100 },
    { id: 2, host_id: 11, host_name: 'b', old_version: '0.3.1', new_version: '0.3.2', new_release: 2000, success: true, created_at: 110 },
    { id: 3, host_id: 10, host_name: 'a', old_version: '0.3.1', new_version: '0.3.2', new_release: 2000, success: true, created_at: 120 },
    { id: 4, host_id: 12, host_name: 'c', old_version: '0.3.1', new_version: '0.3.2', new_release: 2000, success: false, created_at: 125 },
    { id: 5, host_id: 13, host_name: 'd', old_version: '0.3.1', new_version: '0.3.2', new_release: 2000, success: false, created_at: 130 },
  ])
  assert.deepEqual(grouped.allIds, [1, 2, 3, 4, 5])
  assert.deepEqual(grouped.mergedSuccess, {
    version: '0.3.2',
    hostCount: 2, // distinct hosts 10 and 11
    ids: [1, 2, 3],
  })
  assert.equal(grouped.failures.length, 2)
  assert.equal(grouped.failures[0].host_name, 'c')
  assert.equal(grouped.failures[1].host_name, 'd')
}

// 10. Fix Round 1 Item #5: update notice fetch cancels on account switch / sign-out
//     and discards late responses belonging to a previous user (no toast, no markSeen).
{
  const pending = []
  const seenCalls = []
  const emitted = []

  const fetcher = createCorraldNoticeFetcher({
    fetchNotices: (signal) =>
      new Promise((resolve, reject) => {
        pending.push({ signal, resolve, reject })
      }),
    markSeen: (ids) => {
      seenCalls.push(ids)
    },
    onNotices: (grouped) => {
      emitted.push(grouped)
    },
  })

  // User 1 starts a fetch; before it completes, user switches to User 2.
  fetcher.setUser(1)
  const p1 = fetcher.check()
  assert.equal(pending.length, 1)
  assert.equal(pending[0].signal.aborted, false)

  fetcher.setUser(2)
  assert.equal(pending[0].signal.aborted, true, 'account switch must abort previous user request')

  // Even if the stale promise resolves late (ignoring AbortSignal), it must be discarded.
  pending[0].resolve([
    { id: 101, host_id: 1, host_name: 'u1-host', old_version: '0.3.1', new_version: '0.3.2', new_release: 2000, success: true, created_at: 100 },
  ])
  await p1
  assert.equal(seenCalls.length, 0, 'stale user 1 response must NOT call markSeen')
  assert.equal(emitted.length, 0, 'stale user 1 response must NOT emit toast')

  // User 2's own check runs cleanly and delivers User 2's notices.
  const p2 = fetcher.check()
  assert.equal(pending.length, 2)
  assert.equal(pending[1].signal.aborted, false)
  pending[1].resolve([
    { id: 202, host_id: 2, host_name: 'u2-host', old_version: '0.3.1', new_version: '0.3.2', new_release: 2000, success: true, created_at: 200 },
  ])
  await p2
  assert.deepEqual(seenCalls, [[202]])
  assert.equal(emitted.length, 1)
  assert.equal(emitted[0].mergedSuccess?.hostCount, 1)

  // Sign-out (setUser(undefined)) while a refresh request is in flight aborts and discards it.
  const p3 = fetcher.check()
  assert.equal(pending.length, 3)
  fetcher.setUser(undefined)
  assert.equal(pending[2].signal.aborted, true, 'sign-out must abort in-flight notice request')
  pending[2].resolve([
    { id: 203, host_id: 2, host_name: 'u2-host', old_version: '0.3.1', new_version: '0.3.2', new_release: 2000, success: false, created_at: 300 },
  ])
  await p3
  assert.deepEqual(seenCalls, [[202]], 'signed-out late response must NOT call markSeen')
  assert.equal(emitted.length, 1, 'signed-out late response must NOT emit toast')
}

// 11. Fix Round 4 Item #11: render-before-effect account switch window.
//     If the rendered userId changes BEFORE useEffect/useLayoutEffect invokes
//     setUser(newId), getCurrentUserId() catches the switch synchronously so
//     the old user's resolving response is discarded (no toast, no markSeen).
{
  const pending = []
  const seenCalls = []
  const emitted = []
  let renderedUserId = 1

  const fetcher = createCorraldNoticeFetcher({
    getCurrentUserId: () => renderedUserId,
    fetchNotices: (signal) =>
      new Promise((resolve, reject) => {
        pending.push({ signal, resolve, reject })
      }),
    markSeen: (ids) => {
      seenCalls.push(ids)
    },
    onNotices: (grouped) => {
      emitted.push(grouped)
    },
  })

  fetcher.setUser(1)
  const p1 = fetcher.check()
  assert.equal(pending.length, 1)

  // Account switches during React render, BEFORE setUser(2) runs in effect.
  renderedUserId = 2
  pending[0].resolve([
    { id: 301, host_id: 1, host_name: 'u1-secret-host', old_version: '0.3.1', new_version: '0.3.2', new_release: 2000, success: true, created_at: 100 },
  ])
  await p1
  assert.equal(seenCalls.length, 0, 'pre-effect switched user response must NOT call markSeen')
  assert.equal(emitted.length, 0, 'pre-effect switched user response must NOT emit toast')

  // Same for sign-out during render (renderedUserId becomes undefined before effect).
  fetcher.setUser(2)
  const p2 = fetcher.check()
  renderedUserId = undefined
  pending[1].resolve([
    { id: 302, host_id: 2, host_name: 'u2-secret-host', old_version: '0.3.1', new_version: '0.3.2', new_release: 2000, success: false, created_at: 200 },
  ])
  await p2
  assert.equal(seenCalls.length, 0, 'pre-effect signed-out response must NOT call markSeen')
  assert.equal(emitted.length, 0, 'pre-effect signed-out response must NOT emit toast')
}

// Phase 2b: contextual hint message mapping, display gating, and i18n copy presence.
{
  const baseHint = {
    host_id: 7,
    host_name: 'gpu-01',
    reason: 'ssh_session_full',
    op: 'status',
    count: 3,
    last_at: 1_700_000_000,
  }
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'status', count: 3 }), 'corrald.hint.sshSessionStatus')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'status', count: 1 }), 'corrald.hint.sshSessionStatusOne')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'log', count: 2 }), 'corrald.hint.sshSessionLog')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'log', count: 1 }), 'corrald.hint.sshSessionLogOne')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'both', count: 4 }), 'corrald.hint.sshSessionBoth')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'both', count: 1 }), 'corrald.hint.sshSessionBothOne')

  assert.equal(shouldShowCorraldHint(baseHint, { masterEnabled: true, sessionHiddenHostIds: new Set() }), true)
  assert.equal(shouldShowCorraldHint(null, { masterEnabled: true, sessionHiddenHostIds: new Set() }), false)
  assert.equal(shouldShowCorraldHint(baseHint, { masterEnabled: false, sessionHiddenHostIds: new Set() }), false)
  assert.equal(shouldShowCorraldHint(baseHint, { masterEnabled: true, sessionHiddenHostIds: new Set([7]) }), false)
  assert.equal(shouldShowCorraldHint(baseHint, { masterEnabled: true, sessionHiddenHostIds: new Set([8]) }), true)
  assert.equal(shouldShowCorraldHint({ ...baseHint, count: 0 }, { masterEnabled: true, sessionHiddenHostIds: new Set() }), false)
  assert.equal(shouldShowCorraldHint({ ...baseHint, host_id: 0 }, { masterEnabled: true, sessionHiddenHostIds: new Set() }), false)

  // Verify both zh and en i18n keys exist for every Phase 2b hint & setting string.
  const i18nSrc = readFileSync(fileURLToPath(new URL('../src/i18n.ts', import.meta.url)), 'utf8')
  const requiredKeys = [
    'settings.corraldHints',
    'settings.corraldHintsHint',
    'corrald.hint.regionLabel',
    'corrald.hint.enable',
    'corrald.hint.dismiss',
    'corrald.hint.close',
    'corrald.hint.sshSessionStatus',
    'corrald.hint.sshSessionStatusOne',
    'corrald.hint.sshSessionLog',
    'corrald.hint.sshSessionLogOne',
    'corrald.hint.sshSessionBoth',
    'corrald.hint.sshSessionBothOne',
  ]
  for (const key of requiredKeys) {
    const matches = i18nSrc.match(new RegExp(`'${key.replaceAll('.', '\\.')}':`, 'g')) ?? []
    assert.equal(matches.length, 2, `i18n key ${key} must appear in both zh and en tables (found ${matches.length})`)
  }
}

// Phase 2b fix1 item 6: once a hint has been shown in this page session,
// remounting the list (mobile list -> conversation -> back) must not show it again.
{
  const hint = {
    host_id: 7,
    host_name: 'gpu-01',
    reason: 'ssh_session_full',
    op: 'status',
    count: 2,
    last_at: 1_700_000_000,
    generation: 0,
  }
  const shown = new Set([7])
  // Unbroken initial mount keeps the bar visible so the user can read/act on it.
  assert.equal(
    shouldShowCorraldHint(hint, {
      masterEnabled: true,
      sessionHiddenHostIds: new Set(),
      shownHostIds: shown,
      currentlyDisplayedHostId: 7,
    }),
    true,
  )
  // After unmount (currentlyDisplayedHostId becomes null), remounting the list
  // before the next poll MUST NOT render the banner again.
  assert.equal(
    shouldShowCorraldHint(hint, {
      masterEnabled: true,
      sessionHiddenHostIds: new Set(),
      shownHostIds: shown,
      currentlyDisplayedHostId: null,
    }),
    false,
    'item 6: remounting list after hint was shown must suppress re-display',
  )
}

// Phase 2b fix1 item 7: hide the bar immediately when this host's corrald
// state is known to be anything other than off and enablable, and refetch hints
// when consent starts from any entry.
{
  const hint = {
    host_id: 9,
    host_name: 'gpu-02',
    reason: 'ssh_session_full',
    op: 'log',
    count: 3,
    last_at: 1_700_000_000,
    generation: 0,
  }
  assert.equal(isCorraldStatusHintEligible(baseStatus({ state: 'off' })), true)
  for (const st of [
    baseStatus({ state: 'running' }),
    baseStatus({ state: 'installing', op: { id: 'op1', kind: 'install', state: 'running', step: 'upload' } }),
    baseStatus({ state: 'off', admin_disabled: true }),
    baseStatus({ state: 'rebind' }),
    baseStatus({ state: 'incompatible' }),
    baseStatus({ state: 'off', error: { code: 'unsupported_os' } }),
    baseStatus({ state: 'off', op: { id: 'op2', kind: 'install', state: 'failed', error_code: 'unsupported_arch' } }),
  ]) {
    assert.equal(isCorraldStatusHintEligible(st), false)
    assert.equal(
      shouldShowCorraldHint(hint, {
        masterEnabled: true,
        sessionHiddenHostIds: new Set(),
        hostStatus: st,
      }),
      false,
      `item 7: status ${JSON.stringify(st)} must hide hint immediately`,
    )
  }
  assert.equal(
    shouldShowCorraldHint(hint, {
      masterEnabled: true,
      sessionHiddenHostIds: new Set(),
      hostUnavailable: true,
    }),
    false,
    'item 7: unavailable corrald must hide hint immediately',
  )

  const sectionSrc = readFileSync(fileURLToPath(new URL('../src/components/CorraldSection.tsx', import.meta.url)), 'utf8')
  assert.ok(
    sectionSrc.includes('triggerCorraldHintsCheck()'),
    'item 7: CorraldSection must trigger hints refetch when consent flow starts',
  )
  const bannerSrc = readFileSync(fileURLToPath(new URL('../src/components/CorraldHintBanner.tsx', import.meta.url)), 'utf8')
  assert.ok(
    bannerSrc.includes('subscribeHostCorrald') && bannerSrc.includes('subscribeAdminCorraldPolicy'),
    'item 7: CorraldHintBanner must subscribe to live host and admin policy updates',
  )
}

// Phase 2b fix1 item 8: failed shown/dismiss writes retry with backoff, then
// on the next successful hints fetch, and local hidden/shown state survives
// reload via try/catch-guarded storage until the server confirms.
{
  const mem = new Map()
  const storage = {
    getItem: (k) => (mem.has(k) ? mem.get(k) : null),
    setItem: (k, v) => mem.set(k, v),
  }
  const scheduled = []
  let shownAttempts = 0
  let dismissAttempts = 0
  let shownShouldFail = 2 // fail first 2 attempts, succeed on 3rd (2nd retry)
  let dismissShouldFail = 10 // fail all immediate retries; recover later on next fetch

  const sync = createCorraldHintSync({
    storage,
    backoffMs: [100, 300],
    schedule: (fn, delay) => {
      const entry = { fn, delay, cancelled: false }
      scheduled.push(entry)
      return entry
    },
    cancelSchedule: (entry) => {
      entry.cancelled = true
    },
    markShown: async () => {
      shownAttempts++
      if (shownAttempts <= shownShouldFail) throw new Error('offline')
    },
    dismiss: async () => {
      dismissAttempts++
      if (dismissAttempts <= dismissShouldFail) throw new Error('offline')
    },
  })

  sync.setUser(42)
  sync.recordShown(11, 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(shownAttempts, 1)
  assert.equal(scheduled.length, 1)
  assert.equal(scheduled[0].delay, 100)
  assert.equal(sync.getSnapshot().pending.length, 1)

  // Fire 1st backoff retry (100ms) -> still fails, schedules 2nd retry (300ms).
  scheduled[0].fn()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(shownAttempts, 2)
  assert.equal(scheduled.length, 2)
  assert.equal(scheduled[1].delay, 300)

  // Fire 2nd backoff retry (300ms) -> succeeds and clears pending!
  scheduled[1].fn()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(shownAttempts, 3)
  assert.equal(sync.getSnapshot().pending.length, 0)

  // Dismiss host 12 while offline: exhausts backoff retries, stays persisted.
  sync.recordDismiss(12, 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(dismissAttempts, 1)
  scheduled[scheduled.length - 1].fn()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(dismissAttempts, 2)
  scheduled[scheduled.length - 1].fn()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(dismissAttempts, 3)
  assert.equal(sync.getSnapshot().pending.length, 1)
  sync.dispose()

  // Simulate full page reload: new sync instance reads the same storage.
  dismissShouldFail = 0 // network recovered after reload
  const replayed = []
  const syncAfterReload = createCorraldHintSync({
    storage,
    backoffMs: [100, 300],
    markShown: async (id, gen) => {
      replayed.push({ kind: 'shown', id, gen })
    },
    dismiss: async (id, gen) => {
      replayed.push({ kind: 'dismiss', id, gen })
    },
  })
  syncAfterReload.setUser(42)
  const snap = syncAfterReload.getSnapshot()
  assert.equal(snap.hiddenHostIds.has(12), true, 'item 8: hidden host must survive reload before server confirms')
  assert.equal(snap.shownHostIds.has(11), true, 'item 8: shown host must survive reload')
  assert.equal(snap.pending.length, 1)

  // Next successful hints fetch at same generation replays the pending dismiss!
  syncAfterReload.syncServerGeneration(0)
  await Promise.resolve()
  await Promise.resolve()
  assert.deepEqual(replayed, [{ kind: 'dismiss', id: 12, gen: 0 }])
  assert.equal(syncAfterReload.getSnapshot().pending.length, 0)

  // Advancing server generation clears stale local shown/hidden state.
  syncAfterReload.syncServerGeneration(1_700_000_000_500)
  assert.equal(syncAfterReload.getSnapshot().hiddenHostIds.size, 0)
  assert.equal(syncAfterReload.getSnapshot().shownHostIds.size, 0)
  syncAfterReload.dispose()

  // Throwing storage must never crash sync operations.
  const brokenStorage = {
    getItem: () => {
      throw new Error('SecurityError')
    },
    setItem: () => {
      throw new Error('QuotaExceededError')
    },
  }
  const safeSync = createCorraldHintSync({
    storage: brokenStorage,
    markShown: async () => {},
    dismiss: async () => {},
  })
  safeSync.setUser(99)
  safeSync.recordShown(5, 0)
  safeSync.hideLocally(5)
  assert.equal(safeSync.getSnapshot().hiddenHostIds.has(5), true)
  safeSync.dispose()
}

// Phase 2b fix1 item 9: agent picker (narrow desktop windows < 900px) must also
// render the hint banner when the sidebar is not shown, under the same once-only rule.
{
  const convoSrc = readFileSync(fileURLToPath(new URL('../src/terminal/Conversation.tsx', import.meta.url)), 'utf8')
  assert.equal(
    convoSrc.includes('{!onPicked && sideBanner}'),
    false,
    'item 9: AgentList must not unconditionally exclude sideBanner when onPicked is set',
  )
  assert.ok(
    convoSrc.includes('{showHintBanner && sideBanner}'),
    'item 9: AgentList must gate sideBanner via showHintBanner',
  )
  const pickerCalls = convoSrc.match(/<Picker\b[^\n]*showHintBanner=\{!sideShown\}/g) ?? []
  assert.equal(
    pickerCalls.length,
    2,
    'item 9: both Picker call sites must pass showHintBanner={!sideShown}',
  )
}

console.log('test-corrald.mjs: all assertions passed')
