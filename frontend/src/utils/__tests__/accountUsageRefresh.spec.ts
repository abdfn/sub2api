import { describe, expect, it } from 'vitest'
import { buildGrokUsageRefreshKey, buildOpenAIUsageRefreshKey, hasCodexTicketHarvest, shouldReplaceAutoRefreshRow } from '../accountUsageRefresh'
import type { Account } from '@/types'

describe('buildOpenAIUsageRefreshKey', () => {
  it('会在 codex 快照变化时生成不同 key', () => {
    const base = {
      id: 1,
      platform: 'openai',
      type: 'oauth',
      updated_at: '2026-03-07T10:00:00Z',
      last_used_at: '2026-03-07T09:59:00Z',
      extra: {
        codex_usage_updated_at: '2026-03-07T10:00:00Z',
        codex_5h_used_percent: 0,
        codex_7d_used_percent: 0
      }
    } as any

    const next = {
      ...base,
      extra: {
        ...base.extra,
        codex_usage_updated_at: '2026-03-07T10:01:00Z',
        codex_5h_used_percent: 100
      }
    }

    expect(buildOpenAIUsageRefreshKey(base)).not.toBe(buildOpenAIUsageRefreshKey(next))
  })

  it('会在 last_used_at 变化时生成不同 key', () => {
    const base = {
      id: 3,
      platform: 'openai',
      type: 'oauth',
      updated_at: '2026-03-07T10:00:00Z',
      last_used_at: '2026-03-07T10:00:00Z',
      extra: {
        codex_usage_updated_at: '2026-03-07T10:00:00Z',
        codex_5h_used_percent: 12,
        codex_7d_used_percent: 24
      }
    } as any

    const next = {
      ...base,
      last_used_at: '2026-03-07T10:02:00Z'
    }

    expect(buildOpenAIUsageRefreshKey(base)).not.toBe(buildOpenAIUsageRefreshKey(next))
  })

  it('非 OpenAI OAuth 账号返回空 key', () => {
    expect(buildOpenAIUsageRefreshKey({
      id: 2,
      platform: 'anthropic',
      type: 'oauth',
      updated_at: '2026-03-07T10:00:00Z',
      last_used_at: '2026-03-07T10:00:00Z',
      extra: {}
    } as any)).toBe('')
  })
})

describe('buildGrokUsageRefreshKey', () => {
  it('changes when a canonical Grok billing or usage snapshot changes', () => {
    const base = {
      platform: 'grok',
      extra: {
        grok_billing_snapshot: { plan: 'Free', usage_percent: 0 },
        grok_usage_snapshot: { subscription_tier: 'Free', status_code: 200 }
      }
    } as any

    expect(buildGrokUsageRefreshKey(base)).not.toBe(buildGrokUsageRefreshKey({
      ...base,
      extra: {
        ...base.extra,
        grok_billing_snapshot: { plan: 'SuperGrok', usage_percent: 0 }
      }
    }))
    expect(buildGrokUsageRefreshKey(base)).not.toBe(buildGrokUsageRefreshKey({
      ...base,
      extra: {
        ...base.extra,
        grok_usage_snapshot: { subscription_tier: 'SuperGrok', status_code: 200 }
      }
    }))
  })

  it('ignores object key order and a legacy alias shadowed by canonical usage', () => {
    const first = {
      platform: 'grok',
      extra: {
        grok_billing_snapshot: {
          plan: 'SuperGrok',
          limits: { monthly: 100, weekly: 25 }
        },
        grok_usage_snapshot: { status_code: 200, subscription_tier: 'SuperGrok' },
        grok_quota_snapshot: { subscription_tier: 'Free' }
      }
    } as any
    const reordered = {
      platform: 'grok',
      extra: {
        grok_quota_snapshot: { subscription_tier: 'SuperGrok Heavy' },
        grok_usage_snapshot: { subscription_tier: 'SuperGrok', status_code: 200 },
        grok_billing_snapshot: {
          limits: { weekly: 25, monthly: 100 },
          plan: 'SuperGrok'
        }
      }
    } as any

    expect(buildGrokUsageRefreshKey(first)).toBe(buildGrokUsageRefreshKey(reordered))
  })

  it('uses the legacy quota alias only when the canonical snapshot is absent', () => {
    const base = {
      platform: 'grok',
      extra: { grok_quota_snapshot: { subscription_tier: 'Free' } }
    } as any
    const next = {
      platform: 'grok',
      extra: { grok_quota_snapshot: { subscription_tier: 'SuperGrok' } }
    } as any

    expect(buildGrokUsageRefreshKey(base)).not.toBe(buildGrokUsageRefreshKey(next))
  })

  it('tracks the legacy tier when the canonical snapshot has no usable tier', () => {
    for (const canonicalSnapshot of [
      { status_code: 200 },
      { status_code: 200, subscription_tier: '   ' },
    ]) {
      const base = {
        platform: 'grok',
        extra: {
          grok_usage_snapshot: canonicalSnapshot,
          grok_quota_snapshot: { subscription_tier: 'Free' },
        },
      } as any
      const next = {
        ...base,
        extra: {
          ...base.extra,
          grok_quota_snapshot: { subscription_tier: 'SuperGrok' },
        },
      }

      expect(buildGrokUsageRefreshKey(base)).not.toBe(buildGrokUsageRefreshKey(next))
    }
  })

  it('returns an empty key for non-Grok accounts', () => {
    expect(buildGrokUsageRefreshKey({
      platform: 'openai',
      extra: { grok_usage_snapshot: { subscription_tier: 'SuperGrok' } }
    } as any)).toBe('')
  })
})

describe('Codex ticket list updates', () => {
  const base = {
    id: 1, platform: 'openai', type: 'oauth', updated_at: '2026-09-18T00:00:00Z',
    codex_turn_tickets: [{ model: 'gpt-6-astra', ready: false, blocked: false, remaining_seconds: 0, harvest_enabled: true, attempts: 2, harvesting: false }]
  } as Account

  it.each([
    { attempts: 3 },
    { harvesting: true },
    { harvest_enabled: false },
    { next_harvest_at: '2026-09-18T00:00:06Z' },
    { ready: true, remaining_seconds: 60, expires_at: '2026-09-18T00:01:00Z' },
    { blocked: true }
  ])('replaces a changed ticket row without triggering a usage fetch: %o', (change) => {
    const next = { ...base, codex_turn_tickets: [{ ...base.codex_turn_tickets![0], ...change }] }
    expect(shouldReplaceAutoRefreshRow(base, next)).toBe(true)
    expect(buildOpenAIUsageRefreshKey(base)).toBe(buildOpenAIUsageRefreshKey(next))
  })

  it('keeps unchanged rows and detects removed ticket status', () => {
    expect(shouldReplaceAutoRefreshRow(base, structuredClone(base))).toBe(false)
    expect(shouldReplaceAutoRefreshRow(base, { ...base, codex_turn_tickets: [] })).toBe(true)
  })

  it('polls only supported accounts with an enabled ticket harvester', () => {
    expect(hasCodexTicketHarvest(base)).toBe(true)
    expect(hasCodexTicketHarvest({ ...base, type: 'setup-token' })).toBe(true)
    expect(hasCodexTicketHarvest({ ...base, type: 'apikey' })).toBe(false)
    expect(hasCodexTicketHarvest({ ...base, platform: 'anthropic' })).toBe(false)
    expect(hasCodexTicketHarvest({ ...base, codex_turn_tickets: [] })).toBe(false)
    expect(hasCodexTicketHarvest({ ...base, codex_turn_tickets: [{ ...base.codex_turn_tickets![0], harvest_enabled: false }] })).toBe(false)
    expect(hasCodexTicketHarvest({ ...base, codex_turn_tickets: [{ ...base.codex_turn_tickets![0], harvest_enabled: undefined }] })).toBe(false)
  })
})
