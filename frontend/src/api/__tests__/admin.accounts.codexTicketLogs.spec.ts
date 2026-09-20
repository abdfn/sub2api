import { describe, expect, it, vi } from 'vitest'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get } }))

import { accountsAPI, getCodexTicketLogs } from '@/api/admin/accounts'

describe('Codex ticket logs API', () => {
  it('passes the selected model and cancellation signal to the account logs endpoint', async () => {
    const controller = new AbortController()
    const response = { model: 'gpt-5.6-sol', entries: [], status: null, limit: 100 }
    get.mockResolvedValueOnce({ data: response })
    await expect(getCodexTicketLogs(91, 'gpt-5.6-sol', { signal: controller.signal })).resolves.toEqual(response)
    expect(get).toHaveBeenCalledWith('/admin/accounts/91/codex-ticket-logs', {
      params: { model: 'gpt-5.6-sol' }, signal: controller.signal
    })
    expect(accountsAPI.getCodexTicketLogs).toBe(getCodexTicketLogs)
  })
})
