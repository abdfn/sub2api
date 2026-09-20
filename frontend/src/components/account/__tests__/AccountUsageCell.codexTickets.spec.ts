import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import AccountUsageCell from '../AccountUsageCell.vue'
import CodexTicketLogsDialog from '../CodexTicketLogsDialog.vue'
import type { Account } from '@/types'
import zhAccounts from '@/i18n/locales/zh/admin/accounts'
import zhCommon from '@/i18n/locales/zh/common'
import { testMessageCompiler } from '@/__tests__/i18n'

const { getUsage, getCodexTicketLogs } = vi.hoisted(() => ({ getUsage: vi.fn(), getCodexTicketLogs: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getUsage, getCodexTicketLogs } } }))

type Ticket = NonNullable<Account['codex_turn_tickets']>[number]
const startAt = new Date('2026-09-18T00:00:00Z')
// deadline 为打票回归测试提供 deadline 辅助实现
const deadline = (seconds: number) => new Date(startAt.getTime() + seconds * 1000).toISOString()
// ticket 为打票回归测试提供 ticket 辅助实现
const ticket = (overrides: Partial<Ticket> = {}): Ticket => ({
  model: 'gpt-6-astra', ready: false, blocked: false, remaining_seconds: 0,
  harvest_enabled: true, attempts: 3, harvesting: false,
  next_harvest_at: deadline(6), ...overrides
})
const wrappers: VueWrapper[] = []

// renderTickets 为打票回归测试提供 render Tickets 辅助实现
function renderTickets(tickets: Ticket[], type: Account['type'] = 'setup-token', accountOverrides: Partial<Account> = {}) {
  const wrapper = mount(AccountUsageCell, {
    props: {
      account: {
        id: 9821, name: 'Codex', platform: 'openai', type,
        codex_turn_tickets: tickets,
        ...accountOverrides
      } as Account
    },
    global: {
      plugins: [createI18n({ legacy: false, locale: 'zh', messageCompiler: testMessageCompiler, messages: { zh: { ...zhCommon, admin: zhAccounts } } })],
      stubs: { OpenAIQuotaResetCell: true, UsageProgressBar: true, AccountQuotaInfo: true, teleport: true }
    }
  })
  wrappers.push(wrapper)
  return wrapper
}

// updateTickets 为打票回归测试提供 update Tickets 辅助实现
async function updateTickets(wrapper: VueWrapper, tickets: Ticket[]) {
  await wrapper.setProps({ account: { ...wrapper.props('account'), codex_turn_tickets: tickets } })
}

describe('Codex ticket details in the usage window', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(startAt)
    getUsage.mockReset().mockResolvedValue({})
    getCodexTicketLogs.mockReset().mockImplementation((_id, model) => Promise.resolve({ model, entries: [], status: null, limit: 100 }))
  })

  afterEach(() => {
    wrappers.splice(0).forEach((wrapper) => wrapper.unmount())
    vi.useRealTimers()
  })

  it.each(['oauth', 'setup-token'] as const)('shows attempts only when harvesting is enabled with a valid proxy for %s', async (type) => {
    const wrapper = renderTickets([ticket()], type)
    await flushPromises()
    expect(wrapper.text()).toContain('打票 3 次')
    expect(wrapper.text()).toContain('下次打票 00:06')
    expect(wrapper.find('[title*="当前轮次"]').exists()).toBe(true)

    for (const harvest_enabled of [false, undefined]) {
      await updateTickets(wrapper, [ticket({ harvest_enabled })])
      expect(wrapper.text()).not.toContain('打票 3 次')
      expect(wrapper.text()).not.toContain('下次打票')
      expect(wrapper.text()).toContain('暂无有效门票')
    }
  })

  it('counts down per model each second and waits at zero until the server reports an active attempt', async () => {
    const wrapper = renderTickets([
      ticket({ blocked: true }),
      ticket({ model: 'gpt-5.6-sol', attempts: 7, next_harvest_at: deadline(65) })
    ])
    expect(wrapper.text()).toContain('打票 7 次')
    expect(wrapper.text()).toContain('未打到对应 Codex 门票，该模型已暂停')
    expect(wrapper.text()).toContain('下次打票 01:05')

    await vi.advanceTimersByTimeAsync(1000)
    expect(wrapper.text()).toContain('下次打票 00:05')
    expect(wrapper.text()).toContain('下次打票 01:04')
    await vi.advanceTimersByTimeAsync(5000)
    expect(wrapper.text()).toContain('等待打票')
    expect(wrapper.text()).toContain('下次打票 00:59')
    expect(wrapper.text()).not.toContain('下次打票 00:00')
    expect(wrapper.text()).not.toContain('打票中')

    await updateTickets(wrapper, [ticket({ attempts: 4, harvesting: true, next_harvest_at: undefined })])
    expect(wrapper.text()).toContain('打票 4 次')
    expect(wrapper.text()).toContain('打票中')
    expect(wrapper.text()).not.toContain('等待打票')
  })

  it('keeps successful attempts, updates ticket TTL, and accepts a new round without fetching upstream usage', async () => {
    const wrapper = renderTickets([ticket()], 'oauth')
    await flushPromises()
    const originalUsageCalls = getUsage.mock.calls.length
    await updateTickets(wrapper, [ticket({ ready: true, attempts: 5, remaining_seconds: 65, expires_at: deadline(65) })])
    expect(wrapper.text()).toContain('打票 5 次')
    expect(wrapper.text()).toContain('1m05s')
    expect(wrapper.text()).not.toContain('下次打票')
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.text()).toContain('1m03s')

    await updateTickets(wrapper, [ticket({ attempts: 1, harvesting: true })])
    expect(wrapper.text()).toContain('打票 1 次')
    expect(wrapper.text()).toContain('打票中')
    expect(getUsage).toHaveBeenCalledTimes(originalUsageCalls)
  })

  it('shows rate limiting ahead of ticket status and restores status when the account recovers', async () => {
    const wrapper = renderTickets([
      ticket({ blocked: true }),
      ticket({ model: 'gpt-5.6-sol', ready: true, remaining_seconds: 65, expires_at: deadline(65) })
    ], 'setup-token', { rate_limit_reset_at: deadline(2) })
    expect(wrapper.text().match(/限流中，等待恢复/g)).toHaveLength(2)
    expect(wrapper.text()).not.toMatch(/该模型已暂停|等待打票|下次打票|打票中/)

    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.text()).not.toContain('限流中，等待恢复')
    expect(wrapper.text()).toContain('未打到对应 Codex 门票，该模型已暂停')
    expect(wrapper.text()).toContain('下次打票 00:04')
    expect(wrapper.text()).toContain('1m03s')
  })

  it('only shows model rate limiting for the affected ticket and ignores invalid or expired limits', async () => {
    const wrapper = renderTickets([
      ticket({ blocked: true }),
      ticket({ model: 'gpt-5.6-sol', blocked: true })
    ], 'setup-token', {
      rate_limit_reset_at: 'invalid',
      extra: { model_rate_limits: {
        'gpt-6-astra': { rate_limited_at: startAt.toISOString(), rate_limit_reset_at: deadline(2) },
        'gpt-5.6-sol': { rate_limited_at: deadline(-10), rate_limit_reset_at: deadline(-1) }
      } }
    })
    const astraButton = wrapper.get('button[aria-label="查看 gpt-6-astra 的打票日志"]')
    const solButton = wrapper.get('button[aria-label="查看 gpt-5.6-sol 的打票日志"]')
    expect(astraButton.text()).toBe('限流中，等待恢复')
    expect(solButton.text()).toBe('未打到对应 Codex 门票，该模型已暂停')

    await vi.advanceTimersByTimeAsync(2000)
    expect(astraButton.text()).toBe('未打到对应 Codex 门票，该模型已暂停')
  })

  it('shows exhausted quota without a model pause or retry countdown and resumes when harvesting restarts', async () => {
    const wrapper = renderTickets([ticket({ blocked: true })])
    await flushPromises()
    const originalUsageCalls = getUsage.mock.calls.length

    await updateTickets(wrapper, [ticket({ harvest_paused: true, attempts: 4 })])
    expect(wrapper.text()).toContain('额度已满，已停止打票')
    expect(wrapper.text()).toContain('暂无有效门票，仍允许请求')
    expect(wrapper.text()).toContain('打票 4 次')
    expect(wrapper.text()).not.toMatch(/该模型已暂停|等待打票|下次打票|打票中/)
    await vi.advanceTimersByTimeAsync(7000)
    expect(wrapper.text()).not.toMatch(/等待打票|下次打票/)

    await updateTickets(wrapper, [ticket({ harvest_paused: false, next_harvest_at: deadline(10) })])
    expect(wrapper.text()).not.toContain('额度已满，已停止打票')
    expect(wrapper.text()).toContain('下次打票 00:03')
    expect(getUsage).toHaveBeenCalledTimes(originalUsageCalls)
  })

  it('prioritizes invalid tokens on error accounts while keeping ticket logs accessible', async () => {
    const wrapper = renderTickets([ticket({
      token_invalid: true, blocked: true, ready: true, remaining_seconds: 65,
      expires_at: deadline(65), harvest_paused: true, harvesting: true
    })], 'setup-token', {
      status: 'error', error_message: '令牌失效，已停止打票', schedulable: false,
      rate_limit_reset_at: deadline(2)
    })
    const statusButton = wrapper.get('button[aria-label="查看 gpt-6-astra 的打票日志"]')
    expect(statusButton.text()).toBe('令牌失效，已停止打票')
    expect(statusButton.classes()).toContain('text-red-600')
    expect(wrapper.text()).not.toMatch(/限流中|该模型已暂停|额度已满|等待打票|下次打票|打票中|1m05s/)

    await vi.advanceTimersByTimeAsync(7000)
    expect(statusButton.text()).toBe('令牌失效，已停止打票')
    await updateTickets(wrapper, [ticket({ token_invalid: true, harvesting: true })])
    expect(wrapper.text()).not.toMatch(/等待打票|下次打票|打票中/)
    await updateTickets(wrapper, [ticket({ token_invalid: true, next_harvest_at: deadline(15) })])
    expect(wrapper.text()).not.toMatch(/等待打票|下次打票|打票中/)
    await updateTickets(wrapper, [ticket({ token_invalid: true, next_harvest_at: undefined })])
    expect(wrapper.text()).not.toMatch(/等待打票|下次打票|打票中/)

    await statusButton.trigger('click')
    await flushPromises()
    expect(getCodexTicketLogs).toHaveBeenLastCalledWith(9821, 'gpt-6-astra', { signal: expect.any(AbortSignal) })
    await wrapper.get('button[aria-label="Close modal"]').trigger('click')
    await updateTickets(wrapper, [ticket({ token_invalid: true, harvest_enabled: false })])
    const attemptsButton = wrapper.get('button[aria-label="查看 gpt-6-astra 的打票日志（打票 3 次）"]')
    await attemptsButton.trigger('click')
    await flushPromises()
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(2)
    await wrapper.get('button[aria-label="Close modal"]').trigger('click')

    await wrapper.setProps({ account: {
      ...wrapper.props('account'), status: 'active', error_message: null, schedulable: true,
      codex_turn_tickets: [ticket({ token_invalid: false, next_harvest_at: deadline(15) })]
    } })
    expect(wrapper.text()).not.toContain('令牌失效，已停止打票')
    expect(wrapper.text()).toContain('下次打票 00:08')
  })

  it('handles missing, invalid, and elapsed retry deadlines without negative or invalid numbers', async () => {
    const wrapper = renderTickets([ticket()])
    for (const next_harvest_at of [undefined, 'invalid', deadline(-10)]) {
      await updateTickets(wrapper, [ticket({ next_harvest_at, attempts: undefined })])
      expect(wrapper.text()).toContain('打票 0 次')
      expect(wrapper.text()).toContain('等待打票')
      expect(wrapper.text()).not.toMatch(/NaN|Infinity|下次打票/)
    }
  })

  it('decrements legacy TTLs and cleans up the clock on unmount', async () => {
    const originalTimers = vi.getTimerCount()
    const wrapper = renderTickets([ticket({ ready: true, harvest_enabled: undefined, remaining_seconds: 2, expires_at: 'invalid' })])
    expect(wrapper.text()).toContain('0m02s')
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.text()).toContain('0m00s')
    wrapper.unmount()
    wrappers.splice(wrappers.indexOf(wrapper), 1)
    expect(vi.getTimerCount()).toBe(originalTimers)
  })

  it('opens live logs for the exact paused model that was clicked', async () => {
    const wrapper = renderTickets([
      ticket({ blocked: true }),
      ticket({ model: 'gpt-5.6-sol', blocked: true, attempts: 114 })
    ])
    expect(getCodexTicketLogs).not.toHaveBeenCalled()
    const pausedButtons = wrapper.findAll('button').filter((button) => button.text().includes('未打到对应 Codex 门票'))
    expect(pausedButtons).toHaveLength(2)
    await pausedButtons[1].trigger('click')
    await flushPromises()
    expect(wrapper.getComponent(CodexTicketLogsDialog).props('model')).toBe('gpt-5.6-sol')
    expect(getCodexTicketLogs).toHaveBeenLastCalledWith(9821, 'gpt-5.6-sol', { signal: expect.any(AbortSignal) })
    await wrapper.get('button[aria-label="Close modal"]').trigger('click')
    expect(wrapper.findComponent(CodexTicketLogsDialog).exists()).toBe(false)

    await pausedButtons[0].trigger('click')
    await flushPromises()
    expect(getCodexTicketLogs).toHaveBeenLastCalledWith(9821, 'gpt-6-astra', { signal: expect.any(AbortSignal) })
  })

  it('keeps successful ticket attempts clickable for reviewing recent logs', async () => {
    const wrapper = renderTickets([ticket({ ready: true, remaining_seconds: 60, attempts: 115 })])
    const attemptsButton = wrapper.findAll('button').find((button) => button.text() === '打票 115 次')!
    await attemptsButton.trigger('click')
    await flushPromises()
    expect(getCodexTicketLogs).toHaveBeenCalledWith(9821, 'gpt-6-astra', { signal: expect.any(AbortSignal) })
  })
})
