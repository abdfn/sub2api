import { effectScope, ref, type EffectScope } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useAccountListRefresh } from '../useAccountListRefresh'

const scopes: EffectScope[] = []

// startRefresh 为打票回归测试提供 start Refresh 辅助实现
function startRefresh(overrides: { autoEnabled?: boolean; tickets?: boolean; interval?: number } = {}) {
  const blocked = ref(false)
  const state = {
    autoEnabled: ref(overrides.autoEnabled ?? false),
    autoIntervalSeconds: ref(overrides.interval ?? 30),
    autoCountdown: ref(overrides.autoEnabled ? (overrides.interval ?? 30) : 0),
    ticketPollingEnabled: ref(overrides.tickets ?? true),
    silentUntil: ref(0),
    lastRefreshAt: ref(Date.now()),
    isBlocked: () => blocked.value,
    refresh: vi.fn(async () => {})
  }
  const scope = effectScope()
  scopes.push(scope)
  scope.run(() => useAccountListRefresh(state))
  return { ...state, blocked, scope }
}

describe('account list ticket polling', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-09-18T00:00:00Z'))
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
  })
  afterEach(() => {
    scopes.splice(0).forEach((scope) => scope.stop())
    vi.restoreAllMocks()
    vi.useRealTimers()
  })

  it('polls one list every 5 seconds without changing the user preference or refreshing usage statistics', async () => {
    const state = startRefresh()
    await vi.advanceTimersByTimeAsync(4999)
    expect(state.refresh).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(10001)
    expect(state.refresh).toHaveBeenCalledTimes(3)
    expect(state.refresh).toHaveBeenLastCalledWith({ refreshTodayStats: false })
    expect(state.autoEnabled.value).toBe(false)
    expect(state.autoCountdown.value).toBe(0)
  })

  it('pauses on hidden pages, open editors, loading, and the post-edit silent window', async () => {
    const state = startRefresh()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    await vi.advanceTimersByTimeAsync(10000)
    expect(state.refresh).not.toHaveBeenCalled()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    state.blocked.value = true
    await vi.advanceTimersByTimeAsync(10000)
    expect(state.refresh).not.toHaveBeenCalled()
    state.blocked.value = false
    state.silentUntil.value = Date.now() + 5000
    await vi.advanceTimersByTimeAsync(4999)
    expect(state.refresh).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(state.refresh).toHaveBeenCalledTimes(1)
  })

  it('shares requests with automatic refresh while keeping its statistics cadence', async () => {
    const state = startRefresh({ autoEnabled: true, interval: 10 })
    await vi.advanceTimersByTimeAsync(10000)
    expect(state.refresh.mock.calls).toEqual([
      [{ refreshTodayStats: false }],
      [{ refreshTodayStats: true }]
    ])
    expect(state.autoCountdown.value).toBe(10)
  })

  it('waits for an in-flight refresh and stops when no active ticket or automatic refresh needs it', async () => {
    const state = startRefresh()
    let finishRefresh: () => void = () => {}
    state.refresh.mockImplementationOnce(() => new Promise<void>((resolve) => { finishRefresh = resolve }))
    await vi.advanceTimersByTimeAsync(15000)
    expect(state.refresh).toHaveBeenCalledTimes(1)
    finishRefresh()
    await vi.advanceTimersByTimeAsync(1000)
    expect(state.refresh).toHaveBeenCalledTimes(2)
    state.ticketPollingEnabled.value = false
    await vi.advanceTimersByTimeAsync(10000)
    expect(state.refresh).toHaveBeenCalledTimes(2)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('keeps automatic refresh working without ticket polling and clears the timer on disposal', async () => {
    const state = startRefresh({ autoEnabled: true, tickets: false, interval: 5 })
    await vi.advanceTimersByTimeAsync(5000)
    expect(state.refresh).toHaveBeenCalledTimes(1)
    expect(state.refresh).toHaveBeenCalledWith({ refreshTodayStats: true })
    state.scope.stop()
    expect(vi.getTimerCount()).toBe(0)
  })
})
