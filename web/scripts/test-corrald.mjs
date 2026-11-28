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
  canClaimCorraldHint,
  classifyDeleteHostResult,
  corraldFailureKind,
  corraldHintMessageKey,
  corraldSectionModel,
  corraldStatusBarModel,
  createCorraldHintController,
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

// Phase 2b: contextual hint message mapping, pre-claim eligibility, and i18n copy presence.
{
  const baseHint = {
    host_id: 7,
    host_name: 'gpu-01',
    reason: 'ssh_session_full',
    op: 'status',
    count: 3,
    last_at: 1_700_000_000,
    generation: 0,
  }
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'status', count: 3 }), 'corrald.hint.sshSessionStatus')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'status', count: 1 }), 'corrald.hint.sshSessionStatusOne')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'log', count: 2 }), 'corrald.hint.sshSessionLog')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'log', count: 1 }), 'corrald.hint.sshSessionLogOne')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'both', count: 4 }), 'corrald.hint.sshSessionBoth')
  assert.equal(corraldHintMessageKey({ ...baseHint, op: 'both', count: 1 }), 'corrald.hint.sshSessionBothOne')

  assert.equal(canClaimCorraldHint(baseHint, { masterEnabled: true }), true)
  assert.equal(canClaimCorraldHint({ ...baseHint, claimed: true }, { masterEnabled: true }), false)
  assert.equal(canClaimCorraldHint(null, { masterEnabled: true }), false)
  assert.equal(canClaimCorraldHint(baseHint, { masterEnabled: false }), false)
  assert.equal(canClaimCorraldHint(baseHint, { masterEnabled: true, adminDisabled: true }), false)
  assert.equal(canClaimCorraldHint(baseHint, { masterEnabled: true, hostUnavailable: true }), false)
  assert.equal(canClaimCorraldHint({ ...baseHint, count: 0 }, { masterEnabled: true }), false)
  assert.equal(canClaimCorraldHint({ ...baseHint, host_id: 0 }, { masterEnabled: true }), false)

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
      canClaimCorraldHint(baseHint, { masterEnabled: true, hostStatus: st }),
      false,
      `status ${JSON.stringify(st)} must disallow claim`,
    )
  }

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

// Phase 2b fix2: claim-before-display (two tabs racing, claim=false never renders,
// claim failure renders nothing until re-offered, unmount/remount does not re-show,
// closing with X needs no extra write, StrictMode remount preserves active claim).
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

  // Simulated atomic server claim store shared by two tabs.
  const serverClaimed = new Set()
  const claimCalls = []
  const dismissCalls = []
  const atomicClaim = async (hostId, gen) => {
    claimCalls.push({ hostId, gen })
    const key = `${hostId}:${gen}`
    if (serverClaimed.has(key)) return false
    serverClaimed.add(key)
    return true
  }

  const tabA = createCorraldHintController({
    claimShown: atomicClaim,
    dismiss: async (hostId, gen) => dismissCalls.push({ hostId, gen }),
    readCachedStatus: () => undefined,
  })
  const tabB = createCorraldHintController({
    claimShown: atomicClaim,
    dismiss: async (hostId, gen) => dismissCalls.push({ hostId, gen }),
    readCachedStatus: () => undefined,
  })
  tabA.setUser(42)
  tabB.setUser(42)

  // Both narrow-screen tabs fetch the hint while their picker is closed:
  // neither claims and neither displays anything yet.
  tabA.receiveServerHint(hint, 0)
  tabB.receiveServerHint(hint, 0)
  assert.equal(claimCalls.length, 0, 'must not claim before a list surface is mounted')
  assert.equal(tabA.getState().displayedHint, null)
  assert.equal(tabB.getState().displayedHint, null)

  // Tab A opens picker (slot mounts) -> claims atomically -> renders.
  tabA.onSlotMounted()
  assert.equal(tabA.getState().displayedHint, null, 'must not render before claim resolves')
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(tabA.getState().displayedHint?.host_id, 7)

  // Tab B opens picker before next poll -> server returns claimed:false -> Tab B NEVER renders.
  tabB.onSlotMounted()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(tabB.getState().displayedHint, null, 'tab whose claim says false must never render')
  assert.equal(tabB.getState().candidateHint, null)

  // Closing with X after a successful claim hides locally with NO extra network write.
  const claimsBeforeClose = claimCalls.length
  const dismissesBeforeClose = dismissCalls.length
  tabA.closeDisplayed()
  assert.equal(tabA.getState().displayedHint, null)
  assert.equal(claimCalls.length, claimsBeforeClose, 'closing with X must not send another shown claim')
  assert.equal(dismissCalls.length, dismissesBeforeClose, 'closing with X must not send dismiss')

  tabA.dispose()
  tabB.dispose()

  // Simultaneous race: two tabs both call onSlotMounted before either claim resolves.
  const pendingResolvers = []
  let raceWon = false
  const raceA = createCorraldHintController({
    claimShown: () => new Promise((resolve) => pendingResolvers.push(() => {
      if (raceWon) resolve(false)
      else {
        raceWon = true
        resolve(true)
      }
    })),
    dismiss: async () => {},
    readCachedStatus: () => undefined,
  })
  const raceB = createCorraldHintController({
    claimShown: () => new Promise((resolve) => pendingResolvers.push(() => {
      if (raceWon) resolve(false)
      else {
        raceWon = true
        resolve(true)
      }
    })),
    dismiss: async () => {},
    readCachedStatus: () => undefined,
  })
  raceA.setUser(42)
  raceB.setUser(42)
  raceA.receiveServerHint(hint, 0)
  raceB.receiveServerHint(hint, 0)
  raceA.onSlotMounted()
  raceB.onSlotMounted()
  assert.equal(raceA.getState().displayedHint, null)
  assert.equal(raceB.getState().displayedHint, null)
  for (const r of pendingResolvers) r()
  await Promise.resolve()
  await Promise.resolve()
  const shownCount = Number(raceA.getState().displayedHint !== null) + Number(raceB.getState().displayedHint !== null)
  assert.equal(shownCount, 1, 'simultaneous claims across two tabs must render in exactly one tab')

  // Unmounting the list (mobile list -> conversation -> back) clears displayedHint;
  // remounting does not re-show it.
  const winner = raceA.getState().displayedHint ? raceA : raceB
  winner.onSlotUnmounted()
  await Promise.resolve() // flush microtask
  assert.equal(winner.getState().displayedHint, null, 'real unmount must clear displayed banner')
  winner.onSlotMounted()
  await Promise.resolve()
  assert.equal(winner.getState().displayedHint, null, 'remounting list must not re-show banner')
  raceA.dispose()
  raceB.dispose()

  // Claim failure renders nothing; may be offered again on a later fetch.
  let shouldFail = true
  let failAttempts = 0
  const retryable = createCorraldHintController({
    claimShown: async () => {
      failAttempts++
      if (shouldFail) throw new Error('network error')
      return true
    },
    dismiss: async () => {},
    readCachedStatus: () => undefined,
  })
  retryable.setUser(42)
  retryable.onSlotMounted()
  retryable.receiveServerHint(hint, 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(failAttempts, 1)
  assert.equal(retryable.getState().displayedHint, null, 'failed claim must not render banner')

  // StrictMode synchronous unmount+remount during an in-flight claim keeps claim alive without duplicating.
  shouldFail = false
  retryable.receiveServerHint(hint, 0)
  retryable.onSlotUnmounted()
  retryable.onSlotMounted()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(failAttempts, 2, 'StrictMode synchronous remount must not send duplicate claim')
  assert.equal(retryable.getState().displayedHint?.host_id, 7)
  retryable.dispose()
}

// Phase 2b fix2: status turning installing (or any non-off-and-enablable state)
// before the claim -> NO claim, NO render (both controller level and real
// CorraldHintBanner.tsx component level).
{
  const hint = {
    host_id: 11,
    host_name: 'gpu-03',
    reason: 'ssh_session_full',
    op: 'status',
    count: 1,
    last_at: 1_700_000_000,
    generation: 0,
  }
  const cache = new Map()
  let claimCalls = 0
  const ctl = createCorraldHintController({
    claimShown: async () => {
      claimCalls++
      return true
    },
    dismiss: async () => {},
    readCachedStatus: (id) => cache.get(id),
  })
  ctl.setUser(42)

  // Case A: status is already installing in cache when GET resolves -> no claim, no render.
  cache.set(11, { status: baseStatus({ state: 'installing', op: { id: 'op1', kind: 'install', state: 'running', step: 'upload' } }) })
  ctl.receiveServerHint(hint, 0)
  ctl.onSlotMounted()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(claimCalls, 0, 'must not claim when cached status is already installing')
  assert.equal(ctl.getState().displayedHint, null)
  assert.equal(ctl.getState().candidateHint, null)
  ctl.onSlotUnmounted()
  await Promise.resolve()

  // Case B: cache is empty when GET resolves (picker closed), then turns installing
  // right before picker opens (without even firing subscribeHostCorrald) -> no claim, no render.
  cache.clear()
  ctl.receiveServerHint(hint, 0)
  assert.equal(ctl.getState().candidateHint?.host_id, 11)
  cache.set(11, { status: baseStatus({ state: 'installing', op: { id: 'op2', kind: 'install', state: 'running', step: 'start' } }) })
  ctl.onSlotMounted()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(claimCalls, 0, 'pre-claim cache re-check must prevent claim when status turned installing')
  assert.equal(ctl.getState().displayedHint, null)
  assert.equal(ctl.getState().candidateHint, null)
  ctl.onSlotUnmounted()
  await Promise.resolve()

  // Case C: live onHostStatus while claim is in flight cancels rendering.
  cache.clear()
  let resolveClaim
  const ctlInFlight = createCorraldHintController({
    claimShown: () => new Promise((r) => { resolveClaim = r }),
    dismiss: async () => {},
    readCachedStatus: (id) => cache.get(id),
  })
  ctlInFlight.setUser(42)
  ctlInFlight.onSlotMounted()
  ctlInFlight.receiveServerHint(hint, 0)
  ctlInFlight.onHostStatus(11, baseStatus({ state: 'installing', op: { id: 'op3', kind: 'install', state: 'running', step: 'upload' } }), false)
  resolveClaim(true)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(ctlInFlight.getState().displayedHint, null, 'live installing status during in-flight claim must suppress render')
  ctlInFlight.dispose()
  ctl.dispose()

  // Case D: Real CorraldHintBanner.tsx component harness test (reproduces p2b-b P1 #3
  // and verifies claim=false never renders CorraldHintBanner).
  const compileCjs = (relPath) =>
    ts.transpileModule(readFileSync(fileURLToPath(new URL(relPath, import.meta.url)), 'utf8'), {
      compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX },
    }).outputText
  const evalCjs = (code, req) => {
    const m = { exports: {} }
    new Function('require', 'module', 'exports', code)(req, m, m.exports)
    return m.exports
  }
  const logicCjs = evalCjs(compileCjs('../src/corraldLogic.ts'), () => {
    throw new Error('unexpected import in corraldLogic')
  })
  const frames = new Map()
  let curFrame
  let slotIdx = 0
  const enter = (id) => {
    if (!frames.has(id)) frames.set(id, { slots: [], layout: [], passive: [] })
    curFrame = frames.get(id)
    slotIdx = 0
  }
  const depsEqual = (a, b) => a && b && a.length === b.length && a.every((x, i) => Object.is(x, b[i]))
  const makeEffect = (kind) => (fn, deps) => {
    const f = curFrame
    const i = slotIdx++
    const old = f.slots[i]
    if (!old || !depsEqual(old.deps, deps)) {
      f.slots[i] = { deps, cleanup: old?.cleanup }
      f[kind].push(() => {
        old?.cleanup?.()
        f.slots[i].cleanup = fn()
      })
    }
  }
  const reactStub = {
    createContext: (v) => ({ _value: v }),
    useContext: (c) => c._value,
    useState: (init) => {
      const f = curFrame
      const i = slotIdx++
      if (!(i in f.slots)) f.slots[i] = typeof init === 'function' ? init() : init
      return [
        f.slots[i],
        (v) => {
          f.slots[i] = typeof v === 'function' ? v(f.slots[i]) : v
        },
      ]
    },
    useRef: (init) => {
      const i = slotIdx++
      return (curFrame.slots[i] ??= { current: init })
    },
    useCallback: (fn, deps) => {
      const i = slotIdx++
      const old = curFrame.slots[i]
      if (old && depsEqual(old.deps, deps)) return old.fn
      curFrame.slots[i] = { fn, deps }
      return fn
    },
    useEffect: makeEffect('passive'),
    useLayoutEffect: makeEffect('layout'),
  }
  const commit = (id, kind) => {
    const f = frames.get(id)
    if (!f) return
    const q = f[kind].splice(0)
    for (const fn of q) fn()
  }

  const compCache = new Map()
  const compClaimCalls = []
  let compClaimAnswer = false
  let deliverHint
  const prevWindow = globalThis.window
  const prevDocument = globalThis.document
  globalThis.window = { setInterval: () => 1, clearInterval: () => {}, addEventListener: () => {}, removeEventListener: () => {} }
  globalThis.document = { visibilityState: 'visible', addEventListener: () => {}, removeEventListener: () => {} }
  try {
    class FakeApiError extends Error {}
    const modules = {
      react: reactStub,
      'react/jsx-runtime': { jsx: (type, props) => ({ type, props }), jsxs: (type, props) => ({ type, props }) },
      'lucide-react': { X: () => null },
      '../api': { ApiError: FakeApiError, errorText: String },
      '../app': { useSession: () => ({ me: { id: 42 } }) },
      '../corraldApi': {
        CORRALD_HINTS_EVENT: 'corral.corraldHintsRefresh',
        getCorraldHint: () => new Promise((r) => { deliverHint = r }),
        claimCorraldHintShown: async (id, gen) => {
          compClaimCalls.push({ id, gen })
          return compClaimAnswer
        },
        dismissCorraldHint: async () => {},
        readCachedCorrald: (id) => compCache.get(id),
        subscribeHostCorrald: () => () => {},
        subscribeAdminCorraldPolicy: () => () => {},
        triggerCorraldHintsCheck: () => {},
        getHostCorrald: async () => null,
        installHostCorrald: async () => null,
        cancelHostCorrald: async () => null,
        publishHostCorrald: () => {},
      },
      '../corraldLogic': logicCjs,
      '../i18n': { t: (k) => k },
      '../prefs': { usePrefs: () => ({ corraldHints: true }) },
      '../router': { navigate: () => {} },
      './CorraldDialogs': { CorraldConsentAlert: () => null },
      './ui': { useToast: () => () => {} },
    }
    const comp = evalCjs(compileCjs('../src/components/CorraldHintBanner.tsx'), (name) => {
      if (!(name in modules)) throw new Error(`unexpected module ${name}`)
      return modules[name]
    })

    // Subcase D1: delayed GET returns hint while cache ALREADY has installing -> banner is null, 0 claim calls!
    const renderCtl = () => {
      enter('ctl')
      return comp.useCorraldHintController(true)
    }
    renderCtl()
    commit('ctl', 'layout')
    commit('ctl', 'passive')
    compCache.set(11, { status: baseStatus({ state: 'installing', op: { id: 'op1', kind: 'install', state: 'running' } }) })
    deliverHint({ hint, generation: 0 })
    await Promise.resolve()
    await Promise.resolve()
    assert.equal(renderCtl().banner, null, 'component must not return banner when cache already says installing')
    assert.equal(compClaimCalls.length, 0, 'component must not POST claim when cache already says installing')

    // Subcase D2: claim returns false -> slot renders null and never renders CorraldHintBanner.
    frames.clear()
    compCache.clear()
    compClaimAnswer = false
    renderCtl()
    commit('ctl', 'layout')
    commit('ctl', 'passive')
    deliverHint({ hint, generation: 0 })
    await Promise.resolve()
    await Promise.resolve()
    const beforeClaim = renderCtl()
    assert.notEqual(beforeClaim.banner, null)
    enter('slot')
    assert.equal(beforeClaim.banner.type(beforeClaim.banner.props), null, 'slot must render null before claim resolves')
    commit('slot', 'passive')
    await Promise.resolve()
    await Promise.resolve()
    assert.deepEqual(compClaimCalls, [{ id: 11, gen: 0 }])
    assert.equal(renderCtl().banner, null, 'banner must become null when claim returns false')

    // Subcase D3: claim returns true -> slot renders null before resolution, then CorraldHintBanner after!
    frames.clear()
    compClaimCalls.length = 0
    compClaimAnswer = true
    renderCtl()
    commit('ctl', 'layout')
    commit('ctl', 'passive')
    deliverHint({ hint, generation: 0 })
    await Promise.resolve()
    await Promise.resolve()
    const pendingBanner = renderCtl().banner
    enter('slot')
    assert.equal(pendingBanner.type(pendingBanner.props), null, 'slot must render null while claim is in flight')
    commit('slot', 'passive')
    await Promise.resolve()
    await Promise.resolve()
    const claimedBanner = renderCtl().banner
    assert.notEqual(claimedBanner, null)
    enter('slot')
    const rendered = claimedBanner.type(claimedBanner.props)
    assert.equal(rendered.type, comp.CorraldHintBanner, 'slot must render CorraldHintBanner only after claimed:true')

    // Subcase D4 (fix3 item 1, fails on 87306f4): once displayed, if the host's
    // status in the page cache becomes non-off-and-enablable (e.g. running), the
    // displayed banner must hide immediately without waiting for watchedHostId to change.
    compCache.set(11, { status: baseStatus({ state: 'running' }) })
    assert.equal(renderCtl().banner, null, 'fix3 item 1: displayed banner must hide immediately when cache turns running')
    commit('ctl', 'passive')
    compCache.clear()
    assert.equal(renderCtl().banner, null, 'fix3 item 1: hidden displayedHint stays cleared')

    // Subcase D5 (fix4): another tab / reload receiving GET { hint: { ..., claimed: true } }
    // NEVER mounts/renders CorraldHintBanner and NEVER calls claimCorraldHintShown.
    frames.clear()
    compClaimCalls.length = 0
    compClaimAnswer = true
    renderCtl()
    commit('ctl', 'layout')
    commit('ctl', 'passive')
    deliverHint({ hint: { ...hint, claimed: true }, generation: 0 })
    await Promise.resolve()
    await Promise.resolve()
    assert.equal(renderCtl().banner, null, 'fix4: another tab/reload must not render a claimed hint')
    assert.equal(compClaimCalls.length, 0, 'fix4: another tab/reload must not attempt to claim a claimed hint')
  } finally {
    globalThis.window = prevWindow
    globalThis.document = prevDocument
  }
}

// Phase 2b fix4 regression tests (fails on 66e0bdb):
// - After its own claim, a banner survives several normal polls where GET
//   keeps returning { ..., claimed: true } while the host stays eligible.
// - It hides immediately when GET stops returning it (eligibility gone -> null)
//   or returns another host, and stays hidden.
// - Another tab or a page reload receiving { ..., claimed: true } NEVER renders
//   and NEVER attempts to claim.
{
  const h7 = {
    host_id: 7,
    host_name: 'gpu-01',
    reason: 'ssh_session_full',
    op: 'status',
    count: 2,
    last_at: 1_700_000_000,
    generation: 0,
    claimed: false,
  }
  const h8 = {
    host_id: 8,
    host_name: 'gpu-02',
    reason: 'ssh_session_full',
    op: 'log',
    count: 1,
    last_at: 1_700_000_010,
    generation: 0,
    claimed: false,
  }

  let serverClaimed7 = false
  let serverEligible7 = true
  let tabAClaims = 0
  const pollServerFor7 = () => (serverEligible7 ? { ...h7, claimed: serverClaimed7 } : null)

  const tabA = createCorraldHintController({
    claimShown: async () => {
      tabAClaims++
      if (serverClaimed7) return false
      serverClaimed7 = true
      return true
    },
    dismiss: async () => {},
    readCachedStatus: () => undefined,
  })
  tabA.setUser(42)
  tabA.onSlotMounted()
  tabA.receiveServerHint(pollServerFor7(), 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(tabA.getState().displayedHint?.host_id, 7)
  assert.equal(tabAClaims, 1)

  // Normal polls after claim: server returns { ...h7, claimed: true }; banner MUST stay visible!
  for (let i = 0; i < 4; i++) {
    tabA.receiveServerHint(pollServerFor7(), 0)
    assert.equal(tabA.getState().displayedHint?.host_id, 7, `fix4: banner must survive normal poll #${i + 1} after its own claim`)
  }
  assert.equal(tabAClaims, 1, 'fix4: normal polls must not re-claim an already displayed banner')

  // Another tab (or page reload) polling the exact same server response { ...h7, claimed: true }
  // MUST NOT render and MUST NOT call claimShown!
  let tabBClaims = 0
  const tabB = createCorraldHintController({
    claimShown: async () => {
      tabBClaims++
      return false
    },
    dismiss: async () => {},
    readCachedStatus: () => undefined,
  })
  tabB.setUser(42)
  tabB.onSlotMounted()
  tabB.receiveServerHint(pollServerFor7(), 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(tabB.getState().displayedHint, null, 'fix4: another tab/reload must never render a claimed hint')
  assert.equal(tabB.getState().candidateHint, null)
  assert.equal(tabBClaims, 0, 'fix4: another tab/reload must not send POST /hint/shown for a claimed hint')
  tabB.dispose()

  // Eligibility ends on server (e.g. enabled elsewhere / admin disabled) -> GET returns null -> Tab A hides!
  serverEligible7 = false
  tabA.receiveServerHint(pollServerFor7(), 0)
  assert.equal(tabA.getState().displayedHint, null, 'fix4: banner must hide when eligibility ends and GET returns null')

  // Even if server eligibility returns later in same generation (claimed:true), Tab A stays hidden.
  serverEligible7 = true
  tabA.receiveServerHint(pollServerFor7(), 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(tabA.getState().displayedHint, null, 'fix4: once hidden, claimed:true must not re-display the banner')
  assert.equal(tabAClaims, 1)
  tabA.dispose()

  // Also verify closing with X stays hidden on subsequent claimed:true polls.
  const tabClose = createCorraldHintController({
    claimShown: async () => true,
    dismiss: async () => {},
    readCachedStatus: () => undefined,
  })
  tabClose.setUser(42)
  tabClose.onSlotMounted()
  tabClose.receiveServerHint(h8, 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(tabClose.getState().displayedHint?.host_id, 8)
  tabClose.closeDisplayed()
  assert.equal(tabClose.getState().displayedHint, null)
  tabClose.receiveServerHint({ ...h8, claimed: true }, 0)
  assert.equal(tabClose.getState().displayedHint, null, 'fix4: closed banner must not reappear on claimed:true poll')
  tabClose.dispose()
}

// Phase 2b fix3 item 1 regression tests (fails on 87306f4):
// An already-displayed banner must hide when a hints poll no longer returns
// that host, returns it as ineligible in cache, or returns a different host;
// and a hint hidden this way stays shown_at on the server so it never comes back.
{
  const h7 = {
    host_id: 7,
    host_name: 'gpu-01',
    reason: 'ssh_session_full',
    op: 'status',
    count: 2,
    last_at: 1_700_000_000,
    generation: 0,
  }
  const h8 = {
    host_id: 8,
    host_name: 'gpu-02',
    reason: 'ssh_session_full',
    op: 'log',
    count: 1,
    last_at: 1_700_000_010,
    generation: 0,
  }

  // 1a. Poll returning null clears displayedHint, and server shown_at keeps it from returning.
  const serverShown = new Set()
  const cache = new Map()
  const ctl = createCorraldHintController({
    claimShown: async (hostId, gen) => {
      const k = `${hostId}:${gen}`
      if (serverShown.has(k)) return false
      serverShown.add(k)
      return true
    },
    dismiss: async () => {},
    readCachedStatus: (id) => cache.get(id),
  })
  ctl.setUser(42)
  ctl.onSlotMounted()
  ctl.receiveServerHint(h7, 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(ctl.getState().displayedHint?.host_id, 7)

  ctl.receiveServerHint(null, 0)
  assert.equal(ctl.getState().displayedHint, null, 'fix3 item 1: null poll must clear displayedHint')

  // Even if offered again in the same generation, server still has shown_at -> claimed:false -> stays hidden.
  ctl.receiveServerHint(h7, 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(ctl.getState().displayedHint, null, 'fix3 item 1: hint hidden after null poll must not come back')

  // 1b. Displayed banner hides when cached daemon status turns installing even if poll returns the same host.
  ctl.receiveServerHint(h8, 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(ctl.getState().displayedHint?.host_id, 8)
  cache.set(8, { status: baseStatus({ state: 'installing', op: { id: 'op-i', kind: 'install', state: 'running' } }) })
  ctl.receiveServerHint(h8, 0)
  assert.equal(ctl.getState().displayedHint, null, 'fix3 item 1: ineligible cache status must clear displayedHint on poll')

  // 1c. Poll returning a different eligible host clears old displayedHint and claims the new one.
  cache.clear()
  const h9 = { ...h8, host_id: 9, host_name: 'gpu-03' }
  const h10 = { ...h8, host_id: 10, host_name: 'gpu-04' }
  ctl.receiveServerHint(h9, 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(ctl.getState().displayedHint?.host_id, 9)
  ctl.receiveServerHint(h10, 0)
  assert.equal(ctl.getState().displayedHint, null, 'fix3 item 1: poll returning another host must clear previous displayedHint')
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(ctl.getState().displayedHint?.host_id, 10)
  ctl.dispose()
}

// Phase 2b fix2: dismiss retries in memory with backoff, flushes on next
// same-generation fetch, cancels on newer generation, stops on permanent errors.
{
  const scheduled = []
  let dismissAttempts = 0
  let dismissShouldFail = 2 // fail first 2 attempts, succeed on 3rd (2nd backoff retry)
  const dismissedPayloads = []

  const ctl = createCorraldHintController({
    claimShown: async () => true,
    backoffMs: [100, 300],
    schedule: (fn, delay) => {
      const entry = { fn, delay, cancelled: false }
      scheduled.push(entry)
      return entry
    },
    cancelSchedule: (entry) => {
      entry.cancelled = true
    },
    isPermanentError: (err) => err && err.status === 404,
    dismiss: async (hostId, gen) => {
      dismissAttempts++
      dismissedPayloads.push({ hostId, gen })
      if (dismissAttempts <= dismissShouldFail) throw new Error('offline')
    },
    readCachedStatus: () => undefined,
  })
  ctl.setUser(42)

  const h12 = {
    host_id: 12,
    host_name: 'gpu-12',
    reason: 'ssh_session_full',
    op: 'log',
    count: 2,
    last_at: 1_700_000_000,
    generation: 0,
  }
  ctl.dismiss(h12)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(dismissAttempts, 1)
  assert.equal(scheduled.length, 1)
  assert.equal(scheduled[0].delay, 100)
  assert.equal(ctl.getState().pendingDismissCount, 1)

  // 1st backoff retry (100ms) -> still fails, schedules 2nd retry (300ms).
  scheduled[0].fn()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(dismissAttempts, 2)
  assert.equal(scheduled.length, 2)
  assert.equal(scheduled[1].delay, 300)

  // 2nd backoff retry (300ms) -> succeeds and clears pending!
  scheduled[1].fn()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(dismissAttempts, 3)
  assert.equal(ctl.getState().pendingDismissCount, 0)
  assert.deepEqual(dismissedPayloads[2], { hostId: 12, gen: 0 })

  // Exhaust all backoff retries, then recover on next same-generation server fetch.
  dismissShouldFail = 100
  const attemptsBefore = dismissAttempts
  ctl.dismiss({ ...h12, host_id: 13 })
  await Promise.resolve()
  await Promise.resolve()
  scheduled[scheduled.length - 1].fn()
  await Promise.resolve()
  await Promise.resolve()
  scheduled[scheduled.length - 1].fn()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(dismissAttempts, attemptsBefore + 3)
  assert.equal(ctl.getState().pendingDismissCount, 1)

  dismissShouldFail = 0
  ctl.receiveServerHint(null, 0)
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(ctl.getState().pendingDismissCount, 0)

  // Newer generation cancels stale pending dismiss and its retry timer.
  dismissShouldFail = 100
  ctl.dismiss({ ...h12, host_id: 14, generation: 0 })
  await Promise.resolve()
  await Promise.resolve()
  const staleTimer = scheduled[scheduled.length - 1]
  assert.equal(staleTimer.cancelled, false)
  ctl.receiveServerHint(null, 1_700_000_000_500)
  assert.equal(staleTimer.cancelled, true, 'advancing generation must cancel stale dismiss timer')
  assert.equal(ctl.getState().pendingDismissCount, 0)
  ctl.dispose()
}

// Phase 2b fix2: ZERO localStorage / sessionStorage access in hint code.
{
  const bannerSrc = readFileSync(fileURLToPath(new URL('../src/components/CorraldHintBanner.tsx', import.meta.url)), 'utf8')
  const apiSrc = readFileSync(fileURLToPath(new URL('../src/corraldApi.ts', import.meta.url)), 'utf8')
  const hintLogicStart = src.indexOf('export type CorraldHintReason')
  assert.ok(hintLogicStart > 0)
  const hintLogicSrc = src.slice(hintLogicStart)

  for (const [label, code] of [
    ['CorraldHintBanner.tsx', bannerSrc],
    ['corraldApi.ts', apiSrc],
    ['corraldLogic.ts (hint section)', hintLogicSrc],
  ]) {
    assert.equal(
      /\b(?:localStorage|sessionStorage)\b/.test(code),
      false,
      `${label} must not access localStorage or sessionStorage`,
    )
  }

  // Runtime check: running full controller lifecycle with throwing storage globals succeeds.
  const origLS = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  const origSS = Object.getOwnPropertyDescriptor(globalThis, 'sessionStorage')
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    get() {
      throw new Error('localStorage must not be accessed by hint code')
    },
  })
  Object.defineProperty(globalThis, 'sessionStorage', {
    configurable: true,
    get() {
      throw new Error('sessionStorage must not be accessed by hint code')
    },
  })
  try {
    const ctl = createCorraldHintController({
      claimShown: async () => true,
      dismiss: async () => {},
      readCachedStatus: () => undefined,
    })
    ctl.setUser(7)
    ctl.onSlotMounted()
    ctl.receiveServerHint(
      { host_id: 5, host_name: 'box', reason: 'ssh_session_full', op: 'status', count: 1, last_at: 1, generation: 0 },
      0,
    )
    await Promise.resolve()
    await Promise.resolve()
    ctl.dismiss({ host_id: 5, host_name: 'box', reason: 'ssh_session_full', op: 'status', count: 1, last_at: 1, generation: 0 })
    await Promise.resolve()
    ctl.dispose()
  } finally {
    if (origLS) Object.defineProperty(globalThis, 'localStorage', origLS)
    else delete globalThis.localStorage
    if (origSS) Object.defineProperty(globalThis, 'sessionStorage', origSS)
    else delete globalThis.sessionStorage
  }

  const sectionSrc = readFileSync(fileURLToPath(new URL('../src/components/CorraldSection.tsx', import.meta.url)), 'utf8')
  assert.ok(
    sectionSrc.includes('triggerCorraldHintsCheck()'),
    'CorraldSection must trigger hints refetch when consent flow starts',
  )
  assert.ok(
    bannerSrc.includes('subscribeHostCorrald') && bannerSrc.includes('subscribeAdminCorraldPolicy'),
    'CorraldHintBanner must subscribe to live host and admin policy updates',
  )
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
