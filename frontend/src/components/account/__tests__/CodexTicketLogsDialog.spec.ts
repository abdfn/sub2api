import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import CodexTicketLogsDialog from '../CodexTicketLogsDialog.vue'
import type { Account, CodexTicketLogEntry, CodexTicketLogsResponse } from '@/types'
import zhAccounts from '@/i18n/locales/zh/admin/accounts'
import zhCommon from '@/i18n/locales/zh/common'
import enAccounts from '@/i18n/locales/en/admin/accounts'
import enCommon from '@/i18n/locales/en/common'
import { testMessageCompiler } from '@/__tests__/i18n'

const { getCodexTicketLogs } = vi.hoisted(() => ({ getCodexTicketLogs: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getCodexTicketLogs } } }))

const account = { id: 91, name: 'Test Codex' } as Account
// entry 为打票回归测试提供 entry 辅助实现
const entry = (overrides: Partial<CodexTicketLogEntry> = {}): CodexTicketLogEntry => ({
  id: 1, time: '2026-09-19T01:00:00Z', attempt: 114, event: 'miss',
  reason: 'length_mismatch', http_status: 200, ticket_length: 228, target_length: 292,
  duration_ms: 125, ...overrides
})
// payload 为打票回归测试提供 payload 辅助实现
const payload = (overrides: Partial<CodexTicketLogsResponse> = {}): CodexTicketLogsResponse => ({
  model: 'gpt-6-astra', entries: [entry()], limit: 100,
  status: { model: 'gpt-6-astra', ready: false, blocked: true, remaining_seconds: 0, attempts: 114, harvest_enabled: true },
  ...overrides
})
const wrappers: VueWrapper[] = []

// renderDialog 为打票回归测试提供 render Dialog 辅助实现
function renderDialog(props: Partial<InstanceType<typeof CodexTicketLogsDialog>['$props']> = {}, locale = 'zh', realTeleport = false) {
  const wrapper = mount(CodexTicketLogsDialog, {
    attachTo: realTeleport ? document.body : undefined,
    props: { show: true, account, model: 'gpt-6-astra', ...props },
    global: {
      plugins: [createI18n({ legacy: false, locale, messageCompiler: testMessageCompiler, messages: {
        zh: { ...zhCommon, admin: zhAccounts }, en: { ...enCommon, admin: enAccounts }
      } })],
      stubs: { teleport: !realTeleport }
    }
  })
  wrappers.push(wrapper)
  return wrapper
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

describe('CodexTicketLogsDialog', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-09-19T01:00:02Z'))
    getCodexTicketLogs.mockReset().mockResolvedValue(payload())
  })

  afterEach(() => {
    wrappers.splice(0).forEach((wrapper) => wrapper.unmount())
    vi.useRealTimers()
  })

  it('loads the selected account and model immediately, then refreshes the latest events and status', async () => {
    const wrapper = renderDialog()
    await flushPromises()
    expect(getCodexTicketLogs).toHaveBeenCalledWith(91, 'gpt-6-astra', { signal: expect.any(AbortSignal) })
    expect(wrapper.text()).toContain('Test Codex #91')
    expect(wrapper.text()).toContain('打票 114 次')
    expect(wrapper.text()).toContain('门票长度与当前账号的目标长度不符')
    expect(wrapper.text()).toContain('228 / 292')
    expect(wrapper.text()).toContain('125 ms')
    expect(wrapper.text()).toContain('200')

    getCodexTicketLogs.mockResolvedValueOnce(payload({
      entries: [entry(), entry({ id: 2, attempt: 115, event: 'success', reason: 'harvested', ticket_length: 292 })],
      status: { model: 'gpt-6-astra', ready: true, blocked: false, remaining_seconds: 600, attempts: 115, harvest_enabled: true }
    }))
    await vi.advanceTimersByTimeAsync(2000)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('已获得有效门票')
    expect(wrapper.text()).toContain('打票 115 次')
    const rows = wrapper.findAll('tbody tr')
    expect(rows[0].text()).toContain('获得门票')
    expect(rows[0].text()).toContain('292 / 292')
    expect(rows[1].text()).toContain('未命中')
  })

  it('retains existing logs on refresh failure and resumes automatic updates after recovery', async () => {
    const wrapper = renderDialog()
    await flushPromises()
    getCodexTicketLogs.mockRejectedValueOnce(new Error('TOKEN=hidden-sensitive-error'))
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.get('[role="alert"]').text()).toContain('已保留上次日志')
    expect(wrapper.text()).toContain('228 / 292')
    expect(wrapper.text()).not.toContain('hidden-sensitive-error')
    await vi.advanceTimersByTimeAsync(2000)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(3)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it('shows the confirmed egress IP and a flag before the localized country name', async () => {
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [entry({ egress_ip: '203.0.113.42', egress_country_code: 'cn' })] }))
    const wrapper = renderDialog()
    await flushPromises()
    const headers = wrapper.findAll('thead th')
    expect(headers).toHaveLength(9)
    expect(headers[5].text()).toBe('出口 IP')
    expect(headers[6].text()).toBe('出口国家')
    expect(headers[6].attributes('title')).toContain('Country.is')
    const cells = wrapper.get('tbody tr').findAll('td')
    expect(cells[5].text()).toBe('203.0.113.42')
    expect(cells[6].text()).toBe('中国')
    const country = cells[6].get('span')
    expect(country.element.children[0].tagName).toBe('IMG')
    expect(country.element.children[1].textContent).toBe('中国')
    expect(country.get('img').attributes('src')).toBe('https://unpkg.com/flag-icons/flags/4x3/cn.svg')
  })

  it('localizes the country name in English and preserves IPv6 addresses', async () => {
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [entry({ egress_ip: '2001:db8::42', egress_country_code: 'US' })] }))
    const wrapper = renderDialog({}, 'en')
    await flushPromises()
    const cells = wrapper.get('tbody tr').findAll('td')
    expect(cells[5].text()).toBe('2001:db8::42')
    expect(cells[6].text()).toBe('United States')
    expect(cells[6].get('img').attributes('src')).toContain('/us.svg')
  })

  it('opens an honest diagnostic fallback for old records without displaying an unconfirmed country', async () => {
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [entry({ egress_country_code: 'US' })] }))
    const wrapper = renderDialog()
    await flushPromises()
    const cells = wrapper.get('tbody tr').findAll('td')
    expect(cells[5].text()).toBe('失败')
    expect(cells[5].get('button').attributes('title')).toBe('查看失败详情')
    expect(cells[6].text()).toBe('—')
    expect(cells[6].find('img').exists()).toBe(false)
    await cells[5].get('button').trigger('click')
    const detail = wrapper.findAll('[role="dialog"]')[1]
    expect(detail.text()).toContain('该记录未保存出口 IP 探测的具体原因')
    expect(detail.text()).not.toContain('探测接口 HTTP 状态')
  })

  it('keeps only a red failure button in the IP column and shows probe HTTP separately on click', async () => {
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [entry({ http_status: 200, egress_error: { reason: 'http_error', http_status: 403 } })] }))
    const wrapper = renderDialog()
    await flushPromises()
    const cells = wrapper.get('tbody tr').findAll('td')
    const failure = cells[5].get('button')
    expect(cells[4].text()).toBe('200')
    expect(cells[5].text()).toBe('失败')
    expect(failure.classes()).toContain('text-red-600')
    expect(failure.attributes('title')).toBe('查看失败详情')
    expect(wrapper.text()).not.toContain('探测接口返回异常')
    await failure.trigger('click')
    const detail = wrapper.findAll('[role="dialog"]')[1]
    expect(detail.text()).toContain('出口 IP 探测失败')
    expect(detail.text()).toContain('Test Codex #91')
    expect(detail.text()).toContain('gpt-6-astra')
    expect(detail.text()).toContain('轮内次数')
    expect(detail.findAll('dd').map((cell) => cell.text())).toContain('114')
    expect(detail.text()).toContain('记录时间')
    expect(detail.text()).toContain('探测接口返回异常 HTTP 状态')
    expect(detail.text()).toContain('探测接口 HTTP 状态')
    expect(detail.findAll('dd').map((cell) => cell.text())).toContain('403')
    expect(detail.findAll('dd').map((cell) => cell.text())).not.toContain('200')
  })

  it.each([
    ['timeout', '探测最多等待 10 秒'],
    ['network_error', '请检查代理或网络'],
    ['connection_closed', '后续打票需要新建连接'],
    ['unsupported_protocol', '不符合当前连接核对要求'],
    ['ambiguous_ip', '返回了多个 IP'],
    ['connection_changed', '已弃用该探测结果'],
    ['insufficient_time', '剩余时间不超过 20 秒'],
    ['unsupported_transport', '不支持确认同一打票连接']
  ])('explains the recorded egress failure %s without using ticket diagnostics', async (reason, explanation) => {
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [entry({ egress_error: { reason } })] }))
    const wrapper = renderDialog()
    await flushPromises()
    await wrapper.get('tbody tr').findAll('td')[5].get('button').trigger('click')
    const detail = wrapper.findAll('[role="dialog"]')[1]
    expect(detail.text()).toContain(explanation)
    expect(detail.text()).not.toContain('门票长度')
    expect(detail.text()).not.toContain('探测接口 HTTP 状态')
  })

  it('uses a generic localized explanation for unknown egress reasons without exposing raw values', async () => {
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [entry({ egress_error: { reason: 'TOKEN=raw-server-secret' } })] }))
    const wrapper = renderDialog({}, 'en')
    await flushPromises()
    const failure = wrapper.get('tbody tr').findAll('td')[5].get('button')
    expect(failure.text()).toBe('Failed')
    expect(failure.attributes('title')).toBe('View failure details')
    await failure.trigger('click')
    const detail = wrapper.findAll('[role="dialog"]')[1]
    expect(detail.text()).toContain('The egress IP probe failed; no further reason is available.')
    expect(detail.text()).not.toContain('TOKEN')
    expect(detail.text()).not.toContain('raw-server-secret')
  })

  it('distinguishes pending and unattempted probes and gives a known IP precedence over an error', async () => {
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [
      entry({ id: 1, attempt: 0 }),
      entry({ id: 2, event: 'skipped' }),
      entry({ id: 3, reason: 'token_error' }),
      entry({ id: 4, event: 'started', reason: 'request_started' }),
      entry({ id: 5, attempt: 115, egress_ip: '203.0.113.42', egress_country_code: 'US', egress_error: { reason: 'timeout' } })
    ] }))
    const wrapper = renderDialog()
    await flushPromises()
    const cells = wrapper.findAll('tbody tr').map((row) => row.findAll('td')[5])
    expect(cells.map((cell) => cell.text())).toEqual(['203.0.113.42', '探测中', '—', '—', '—'])
    expect(cells.every((cell) => !cell.find('button').exists())).toBe(true)
  })

  it('pairs completed attempts with their start without borrowing results from another round', async () => {
    const firstStart = entry({ id: 1, attempt: 1, event: 'started', reason: 'request_started' })
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [firstStart] }))
    const wrapper = renderDialog()
    await flushPromises()
    const startRow = wrapper.get('tbody tr').element
    expect(wrapper.get('tbody tr').findAll('td')[5].text()).toBe('探测中')
    const result = entry({ id: 2, attempt: 1, egress_ip: '203.0.113.42', egress_country_code: 'US' })
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [
      firstStart, result,
      entry({ id: 3, attempt: 1, event: 'started', reason: 'request_started' }),
      entry({ id: 4, attempt: 1, egress_error: { reason: 'timeout' } }),
      entry({ id: 5, attempt: 1, event: 'started', reason: 'request_started' })
    ] }))
    await vi.advanceTimersByTimeAsync(2000)
    const rows = wrapper.findAll('tbody tr')
    expect(rows.map((row) => row.findAll('td')[5].text())).toEqual(['探测中', '失败', '失败', '203.0.113.42', '203.0.113.42'])
    expect(rows[4].element).toBe(startRow)
    expect(rows[4].findAll('td')[6].text()).toBe('美国')
  })

  it('retains the selected diagnostic and log scroll during polling and after its record is evicted', async () => {
    const original = entry({ egress_error: { reason: 'timeout' } })
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [original] }))
    const wrapper = renderDialog()
    await flushPromises()
    const table = wrapper.get('table').element
    const row = wrapper.get('tbody tr').element
    const scroller = table.parentElement!
    scroller.scrollTop = 80
    scroller.scrollLeft = 120
    await wrapper.get('tbody tr').findAll('td')[5].get('button').trigger('click')
    const detail = wrapper.findAll('[role="dialog"]')[1].element
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [{ ...original, egress_error: { reason: 'network_error' } }] }))
    await vi.advanceTimersByTimeAsync(2000)
    await wrapper.setProps({ account: { ...account, name: 'Updated account' } })
    expect(wrapper.get('tbody tr').element).toBe(row)
    expect(wrapper.findAll('[role="dialog"]')[1].element).toBe(detail)
    expect(wrapper.findAll('[role="dialog"]')[1].text()).toContain('探测最多等待 10 秒')
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [entry({ id: 2, attempt: 115, egress_ip: '203.0.113.43' })] }))
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.get('table').element).toBe(table)
    expect(scroller.scrollTop).toBe(80)
    expect(scroller.scrollLeft).toBe(120)
    expect(wrapper.findAll('[role="dialog"]')[1].element).toBe(detail)
    expect(wrapper.findAll('[role="dialog"]')[1].text()).toContain('探测最多等待 10 秒')
    expect(wrapper.findAll('[role="dialog"]')[1].text()).toContain('Test Codex #91')
  })

  it.each(['escape', 'header', 'footer'])('closes only the upper dialog via %s and preserves the log body lock', async (action) => {
    const wrapper = renderDialog()
    await flushPromises()
    const table = wrapper.get('table').element
    const scroller = table.parentElement!
    scroller.scrollTop = 80
    await wrapper.get('tbody tr').findAll('td')[5].get('button').trigger('click')
    await flushPromises()
    expect(wrapper.get('table').element).toBe(table)
    expect(document.body.classList.contains('modal-open')).toBe(true)
    const detail = wrapper.findAll('[role="dialog"]')[1]
    if (action === 'escape') document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    else if (action === 'header') await detail.get('button[aria-label="Close modal"]').trigger('click')
    else await detail.get('.modal-footer button').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('[role="dialog"]')).toHaveLength(1)
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(document.body.classList.contains('modal-open')).toBe(true)
    expect(wrapper.get('table').element).toBe(table)
    expect(scroller.scrollTop).toBe(80)
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await flushPromises()
    expect(wrapper.emitted('close')).toHaveLength(1)
    await wrapper.setProps({ show: false })
    expect(document.body.classList.contains('modal-open')).toBe(false)
  })

  it('preserves the real teleported log DOM and restores focus and scroll lock after closing details', async () => {
    const wrapper = renderDialog({}, 'zh', true)
    await flushPromises()
    const logDialog = document.querySelector<HTMLElement>('[role="dialog"]')!
    const table = logDialog.querySelector('table')!
    const scroller = table.parentElement!
    const modalBody = logDialog.querySelector<HTMLElement>('.modal-body')!
    const button = logDialog.querySelector<HTMLButtonElement>('tbody td:nth-child(6) button')!
    scroller.scrollTop = 80
    scroller.scrollLeft = 120
    modalBody.scrollTop = 40
    button.focus()
    button.click()
    await flushPromises()
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(2)
    expect(logDialog.querySelector('table')).toBe(table)
    const details = document.querySelectorAll<HTMLElement>('[role="dialog"]')[1]
    expect(details.contains(document.activeElement)).toBe(true)
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await flushPromises()
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(1)
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(logDialog.querySelector('table')).toBe(table)
    expect(document.activeElement).toBe(button)
    expect(document.body.classList.contains('modal-open')).toBe(true)
    expect(scroller.scrollTop).toBe(80)
    expect(scroller.scrollLeft).toBe(120)
    expect(modalBody.scrollTop).toBe(40)
    await wrapper.setProps({ show: false })
    expect(document.body.classList.contains('modal-open')).toBe(false)
  })

  it.each(['account', 'model', 'closed'])('clears selected failure details when the log identity changes (%s)', async (change) => {
    const wrapper = renderDialog()
    await flushPromises()
    await wrapper.get('tbody tr').findAll('td')[5].get('button').trigger('click')
    expect(wrapper.findAll('[role="dialog"]')).toHaveLength(2)
    if (change === 'account') await wrapper.setProps({ account: { ...account, id: 92 } })
    else if (change === 'model') {
      getCodexTicketLogs.mockResolvedValueOnce(payload({ model: 'gpt-5.6-sol' }))
      await wrapper.setProps({ model: 'gpt-5.6-sol' })
    } else await wrapper.setProps({ show: false })
    await flushPromises()
    expect(wrapper.findAll('[role="dialog"]')).toHaveLength(change === 'closed' ? 0 : 1)
    expect(wrapper.text()).not.toContain('该记录未保存出口 IP 探测的具体原因')
    expect(document.body.classList.contains('modal-open')).toBe(change !== 'closed')
  })

  it('does not create flags for missing, unknown, or malformed country codes', async () => {
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [undefined, '', 'ZZ', 'XX', 'USA', '../us', 'EU']
      .map((code, index) => entry({ id: index + 1, egress_ip: '203.0.113.42', egress_country_code: code })) }))
    const wrapper = renderDialog()
    await flushPromises()
    expect(wrapper.findAll('tbody tr')).toHaveLength(7)
    for (const row of wrapper.findAll('tbody tr')) {
      const cells = row.findAll('td')
      expect(cells[5].text()).toBe('203.0.113.42')
      expect(cells[6].text()).toBe('—')
      expect(cells[6].get('span').attributes('title')).toContain('国家信息暂未获取')
      expect(cells[6].find('img').exists()).toBe(false)
    }
  })

  it('fills in country information on the next poll without replacing the row or resetting scroll', async () => {
    const first = entry({ egress_ip: '203.0.113.42' })
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [first] }))
    const wrapper = renderDialog()
    await flushPromises()
    const table = wrapper.get('table').element
    const row = wrapper.get('tbody tr').element
    const scroller = table.parentElement!
    scroller.scrollTop = 60
    scroller.scrollLeft = 100
    const pending = deferred<CodexTicketLogsResponse>()
    getCodexTicketLogs.mockReturnValueOnce(pending.promise)
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.get('tbody tr').element).toBe(row)
    expect(wrapper.get('tbody tr').findAll('td')[6].text()).toBe('—')
    const enriched = payload({ entries: [{ ...first, egress_country_code: 'US' }] })
    pending.resolve(enriched)
    await flushPromises()
    expect(wrapper.get('table').element).toBe(table)
    expect(wrapper.get('tbody tr').element).toBe(row)
    expect(wrapper.get('tbody tr').findAll('td')[6].text()).toBe('美国')
    expect(scroller.scrollTop).toBe(60)
    expect(scroller.scrollLeft).toBe(100)
    const flag = wrapper.get('tbody img').element
    getCodexTicketLogs.mockResolvedValueOnce(enriched)
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.get('tbody img').element).toBe(flag)
    expect(wrapper.get('tbody tr').element).toBe(row)
  })

  it('keeps logs and the polling schedule when the account list replaces the same account', async () => {
    const wrapper = renderDialog()
    await flushPromises()
    const table = wrapper.get('table').element
    const row = wrapper.get('tbody tr').element
    const scroller = table.parentElement!
    scroller.scrollTop = 80
    const pending = deferred<CodexTicketLogsResponse>()
    getCodexTicketLogs.mockReturnValueOnce(pending.promise)

    await vi.advanceTimersByTimeAsync(1000)
    await wrapper.setProps({ account: { ...account, name: 'Updated account' } as Account })
    expect(wrapper.text()).toContain('Updated account #91')
    expect(wrapper.find('table').element).toBe(table)
    expect(wrapper.find('tbody tr').element).toBe(row)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(1000)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(2)
    const signal = getCodexTicketLogs.mock.calls[1][2].signal as AbortSignal
    await wrapper.setProps({ account: { ...account, name: 'Another account snapshot' } as Account })
    expect(signal.aborted).toBe(false)
    expect(wrapper.find('table').element).toBe(table)
    expect(wrapper.find('tbody tr').element).toBe(row)
    expect(scroller.scrollTop).toBe(80)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(2)
    const refreshButton = wrapper.get('.modal-footer .btn-secondary')
    expect(refreshButton.attributes('disabled')).toBeUndefined()
    await refreshButton.trigger('click')
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(2)

    pending.resolve(payload({ entries: [entry(), entry({ id: 2, attempt: 115, reason: 'timeout' })] }))
    await flushPromises()
    expect(wrapper.get('table').element).toBe(table)
    expect(wrapper.findAll('tbody tr')[1].element).toBe(row)
    expect(wrapper.text()).toContain('打票请求超时')
    await vi.advanceTimersByTimeAsync(2000)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(3)
  })

  it('explains quota exhaustion and keeps model requests allowed when ticket harvesting stops', async () => {
    const wrapper = renderDialog()
    await flushPromises()
    expect(wrapper.text()).toContain('该模型已暂停')

    getCodexTicketLogs.mockResolvedValueOnce(payload({
      entries: [entry({ reason: 'quota_exhausted', http_status: 429, ticket_length: undefined })],
      status: {
        model: 'gpt-6-astra', ready: false, blocked: false, remaining_seconds: 0,
        attempts: 114, harvest_enabled: true, harvest_paused: true, harvesting: false,
        next_harvest_at: '2026-09-19T01:00:10Z'
      }
    }))
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.text()).toContain('额度已满，已停止打票')
    expect(wrapper.text()).toContain('暂无有效门票，仍允许请求')
    expect(wrapper.text()).not.toMatch(/该模型已暂停|等待打票|下次打票|打票中/)
    const row = wrapper.get('tbody tr')
    expect(row.text()).toContain('未命中')
    expect(row.text()).toContain('打票返回 429，且 5h 或 7d 用量已达到 100%，已停止打票，仍允许模型请求')
    expect(row.findAll('td')[4].text()).toBe('429')
  })

  it.each([
    ['zh', '令牌失效，已停止打票', '打票 114 次'],
    ['en', 'Token invalid; ticket harvesting stopped', '114 ticket attempts']
  ])('shows the invalid-token status and HTTP 401 reason for error accounts in %s', async (locale, label, attempts) => {
    const invalidStatus = {
      model: 'gpt-6-astra', ready: true, blocked: true, remaining_seconds: 600,
      attempts: 114, token_invalid: true, harvest_enabled: true, harvest_paused: true,
      harvesting: true, next_harvest_at: '2026-09-19T01:00:10Z'
    }
    const invalidPayload = payload({
      status: invalidStatus,
      entries: [entry({ event: 'error', reason: 'token_invalid', http_status: 401, ticket_length: undefined })]
    })
    getCodexTicketLogs.mockResolvedValue(invalidPayload)
    const wrapper = renderDialog({ account: {
      ...account, status: 'error', error_message: '令牌失效，已停止打票', schedulable: false,
      rate_limit_reset_at: '2026-09-19T01:00:04Z'
    } }, locale)
    await flushPromises()
    const summary = wrapper.get('.mt-3')
    expect(summary.text()).toContain(label)
    expect(summary.text()).toContain(attempts)
    expect(summary.get('span').classes()).toContain('text-red-600')
    expect(summary.text()).not.toMatch(/限流中|该模型已暂停|额度已满|等待打票|下次打票|打票中|已获得有效门票|Rate limited|paused|Quota exhausted|Waiting|Next attempt|Getting ticket|Valid ticket/)
    const row = wrapper.get('tbody tr')
    expect(row.findAll('td')[3].text()).toBe(label)
    expect(row.findAll('td')[4].text()).toBe('401')

    for (const harvesting of [true, false]) {
      getCodexTicketLogs.mockResolvedValue(payload({
        ...invalidPayload,
        status: { ...invalidStatus, ready: false, harvest_paused: false, harvesting }
      }))
      await vi.advanceTimersByTimeAsync(2000)
      expect(summary.text()).toContain(label)
      expect(summary.text()).not.toMatch(/额度已满|下次打票|打票中|Quota exhausted|Next attempt|Getting ticket/)
    }
  })

  it.each<Partial<Account>>([
    { rate_limit_reset_at: '2026-09-19T01:00:04Z' },
    { extra: { model_rate_limits: {
      'gpt-6-astra': { rate_limited_at: '2026-09-19T01:00:00Z', rate_limit_reset_at: '2026-09-19T01:00:04Z' }
    } } }
  ])('shows rate limiting until recovery in the live log status (%j)', async (rateLimit) => {
    const result = payload()
    result.status!.next_harvest_at = '2026-09-19T01:00:10Z'
    getCodexTicketLogs.mockResolvedValue(result)
    const wrapper = renderDialog({ account: { ...account, ...rateLimit } })
    await flushPromises()
    expect(wrapper.text()).toContain('限流中，等待恢复')
    expect(wrapper.text()).not.toMatch(/该模型已暂停|下次打票|打票中/)

    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.text()).not.toContain('限流中，等待恢复')
    expect(wrapper.text()).toContain('未打到对应 Codex 门票，该模型已暂停')
    expect(wrapper.text()).toContain('下次打票')
  })

  it('offers retry after an initial failure and explains the empty log window', async () => {
    getCodexTicketLogs.mockRejectedValueOnce(new Error('offline'))
    const wrapper = renderDialog()
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('日志加载失败')
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [], status: null }))
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('暂无打票日志')
    expect(wrapper.text()).toContain('最近 100 条事件')
    expect(wrapper.text()).toContain('服务重启后清空')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it('does not overlap slow requests and cancels the request and polling on close', async () => {
    const pending = deferred<CodexTicketLogsResponse>()
    getCodexTicketLogs.mockReturnValueOnce(pending.promise)
    const wrapper = renderDialog()
    const signal = getCodexTicketLogs.mock.calls[0][2].signal as AbortSignal
    await vi.advanceTimersByTimeAsync(10000)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(1)
    await wrapper.get('button[aria-label="Close modal"]').trigger('click')
    expect(wrapper.emitted('close')).toHaveLength(1)
    expect(signal.aborted).toBe(true)
    pending.resolve(payload())
    await flushPromises()
    await vi.advanceTimersByTimeAsync(10000)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(1)
    expect(wrapper.find('tbody').exists()).toBe(false)
  })

  it('clears polling on unmount and cancels an in-flight refresh', async () => {
    const originalTimers = vi.getTimerCount()
    const wrapper = renderDialog()
    await flushPromises()
    const pending = deferred<CodexTicketLogsResponse>()
    getCodexTicketLogs.mockReturnValueOnce(pending.promise)
    await vi.advanceTimersByTimeAsync(2000)
    const signal = getCodexTicketLogs.mock.calls[1][2].signal as AbortSignal
    wrapper.unmount()
    wrappers.splice(wrappers.indexOf(wrapper), 1)
    expect(signal.aborted).toBe(true)
    pending.resolve(payload())
    await flushPromises()
    await vi.advanceTimersByTimeAsync(6000)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(2)
    expect(vi.getTimerCount()).toBe(originalTimers)
  })

  it('only fetches while open and reloads after reopening', async () => {
    const wrapper = renderDialog({ show: false })
    await vi.advanceTimersByTimeAsync(6000)
    expect(getCodexTicketLogs).not.toHaveBeenCalled()
    await wrapper.setProps({ show: true })
    await flushPromises()
    await wrapper.setProps({ show: false })
    await vi.advanceTimersByTimeAsync(6000)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(1)
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(2)
  })

  it('cancels old requests and ignores stale results and errors when switching account or model', async () => {
    const oldModel = deferred<CodexTicketLogsResponse>()
    const oldAccount = deferred<CodexTicketLogsResponse>()
    getCodexTicketLogs.mockReturnValueOnce(oldModel.promise).mockReturnValueOnce(oldAccount.promise)
    const wrapper = renderDialog()
    const firstSignal = getCodexTicketLogs.mock.calls[0][2].signal as AbortSignal
    await wrapper.setProps({ model: 'gpt-5.6-sol' })
    const secondSignal = getCodexTicketLogs.mock.calls[1][2].signal as AbortSignal
    getCodexTicketLogs.mockResolvedValueOnce(payload({ model: 'gpt-5.6-sol', entries: [entry({ event: 'error', reason: 'timeout', duration_ms: 30000 })], status: null }))
    await wrapper.setProps({ account: { ...account, id: 92, name: 'Other account' } as Account })
    await flushPromises()
    expect(getCodexTicketLogs).toHaveBeenLastCalledWith(92, 'gpt-5.6-sol', { signal: expect.any(AbortSignal) })
    expect(firstSignal.aborted).toBe(true)
    expect(secondSignal.aborted).toBe(true)
    oldModel.resolve(payload())
    oldAccount.reject(new Error('stale request failed'))
    await flushPromises()
    expect(wrapper.text()).toContain('Other account #92')
    expect(wrapper.text()).toContain('打票请求超时')
    expect(wrapper.text()).not.toContain('门票长度与当前账号的目标长度不符')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    getCodexTicketLogs.mockResolvedValue(payload({ model: 'gpt-5.6-sol', entries: [], status: null }))
    await vi.advanceTimersByTimeAsync(2000)
    expect(getCodexTicketLogs).toHaveBeenCalledTimes(4)
  })

  it('clears the previous model logs while the new model loads', async () => {
    const wrapper = renderDialog()
    await flushPromises()
    const pending = deferred<CodexTicketLogsResponse>()
    getCodexTicketLogs.mockReturnValueOnce(pending.promise)
    await wrapper.setProps({ model: 'gpt-5.6-sol' })
    expect(wrapper.text()).not.toContain('228 / 292')
    expect(wrapper.find('tbody').exists()).toBe(false)
    pending.resolve(payload({ model: 'gpt-5.6-sol', entries: [], status: null }))
    await flushPromises()
    expect(wrapper.text()).toContain('暂无打票日志')
  })

  it('uses generic labels for unknown reasons and preserves zero measurements', async () => {
    getCodexTicketLogs.mockResolvedValueOnce(payload({ entries: [entry({ reason: 'raw TOKEN secret', ticket_length: 0, duration_ms: 0 })] }))
    const wrapper = renderDialog()
    await flushPromises()
    expect(wrapper.text()).toContain('打票未完成，暂无进一步原因')
    expect(wrapper.text()).not.toContain('TOKEN')
    expect(wrapper.text()).toContain('0 / 292')
    expect(wrapper.text()).toContain('0 ms')
  })
})
