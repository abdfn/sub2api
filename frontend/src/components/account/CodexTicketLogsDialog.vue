<template>
  <BaseDialog :show="show" :title="t('admin.accounts.openai.codexTicketLogs.title')" width="extra-wide" :close-on-escape="false" @close="close">
    <div class="space-y-4">
      <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-800">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div class="min-w-0">
            <div class="break-words text-sm font-medium text-gray-900 dark:text-gray-100">{{ account.name }} <span class="font-normal text-gray-500">#{{ account.id }}</span></div>
            <div class="mt-1 break-all font-mono text-xs text-gray-600 dark:text-gray-400">{{ model }}</div>
          </div>
          <span class="inline-flex items-center gap-1.5 text-xs" :class="failed ? 'text-amber-600 dark:text-amber-400' : 'text-emerald-600 dark:text-emerald-400'" role="status">
            <span class="h-1.5 w-1.5 rounded-full bg-current"></span>
            {{ failed ? t('admin.accounts.openai.codexTicketLogs.retrying') : t('admin.accounts.openai.codexTicketLogs.live') }}
          </span>
        </div>
        <div v-if="status" class="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
          <span :class="status.token_invalid ? 'text-red-600 dark:text-red-400' : rateLimited ? 'text-amber-600 dark:text-amber-400' : status.ready ? 'text-emerald-600 dark:text-emerald-400' : status.blocked ? 'text-amber-600 dark:text-amber-400' : 'text-gray-600 dark:text-gray-400'">{{ statusLabel }}</span>
          <span class="text-gray-600 dark:text-gray-400">{{ t('admin.accounts.openai.codexTurnTicketAttempts', { count: status.attempts ?? 0 }) }}</span>
          <template v-if="!status.token_invalid">
            <span v-if="status.harvest_paused" class="text-gray-500">{{ t('admin.accounts.openai.codexTurnTicketHarvestPaused') }}</span>
            <span v-else-if="status.harvesting && !rateLimited" class="text-blue-600 dark:text-blue-400">{{ t('admin.accounts.openai.codexTurnTicketHarvesting') }}</span>
            <span v-else-if="!status.harvest_enabled" class="text-gray-500">{{ t('admin.accounts.openai.codexTicketLogs.disabled') }}</span>
            <span v-else-if="!status.ready && !rateLimited && status.next_harvest_at" class="text-gray-500">{{ t('admin.accounts.openai.codexTicketLogs.nextAttempt', { time: formatTime(status.next_harvest_at) }) }}</span>
          </template>
        </div>
      </div>

      <div v-if="failed" class="flex items-center justify-between gap-3 rounded-lg bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:bg-amber-900/20 dark:text-amber-300" role="alert">
        <span>{{ response ? t('admin.accounts.openai.codexTicketLogs.refreshFailed') : t('admin.accounts.openai.codexTicketLogs.loadFailed') }}</span>
        <button type="button" class="shrink-0 rounded px-2 py-1 font-medium underline disabled:opacity-50" :disabled="initialLoading" @click="refresh">{{ t('admin.accounts.openai.codexTicketLogs.retry') }}</button>
      </div>

      <div v-if="initialLoading" class="py-10 text-center text-sm text-gray-500" role="status">{{ t('common.loading') }}</div>
      <div v-else-if="response && !entries.length" class="rounded-lg border border-dashed border-gray-200 p-8 text-center text-sm text-gray-500 dark:border-dark-600">{{ t('admin.accounts.openai.codexTicketLogs.empty') }}</div>
      <div v-if="entries.length" class="max-h-[50vh] overflow-auto rounded-lg border border-gray-200 dark:border-dark-600">
        <table class="w-full min-w-[1000px] text-left text-xs">
          <thead class="sticky top-0 bg-gray-50 text-gray-500 dark:bg-dark-800 dark:text-gray-400">
            <tr>
              <th scope="col" class="px-3 py-2 font-medium">{{ t('admin.accounts.openai.codexTicketLogs.time') }}</th>
              <th scope="col" class="px-3 py-2 font-medium">{{ t('admin.accounts.openai.codexTicketLogs.attempt') }}</th>
              <th scope="col" class="px-3 py-2 font-medium">{{ t('admin.accounts.openai.codexTicketLogs.result') }}</th>
              <th scope="col" class="px-3 py-2 font-medium">{{ t('admin.accounts.openai.codexTicketLogs.reason') }}</th>
              <th scope="col" class="px-3 py-2 font-medium">HTTP</th>
              <th scope="col" class="px-3 py-2 font-medium">{{ t('admin.accounts.openai.codexTicketLogs.egressIp') }}</th>
              <th scope="col" class="px-3 py-2 font-medium" :title="t('admin.accounts.openai.codexTicketLogs.egressCountryHint')">{{ t('admin.accounts.openai.codexTicketLogs.egressCountry') }}</th>
              <th scope="col" class="px-3 py-2 font-medium">{{ t('admin.accounts.openai.codexTicketLogs.length') }}</th>
              <th scope="col" class="px-3 py-2 font-medium">{{ t('admin.accounts.openai.codexTicketLogs.duration') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 text-gray-700 dark:divide-dark-700 dark:text-gray-300">
            <tr v-for="entry in entries" :key="entry.id">
              <td class="whitespace-nowrap px-3 py-2.5 tabular-nums">{{ formatTime(entry.time) }}</td>
              <td class="px-3 py-2.5 tabular-nums">{{ entry.attempt }}</td>
              <td class="whitespace-nowrap px-3 py-2.5" :class="eventClass(entry.event)">{{ eventLabel(entry.event) }}</td>
              <td class="min-w-[180px] px-3 py-2.5">{{ reasonLabel(entry.reason) }}</td>
              <td class="px-3 py-2.5 tabular-nums">{{ entry.http_status ?? '—' }}</td>
              <td class="whitespace-nowrap px-3 py-2.5">
                <span v-if="entry.egress.state === 'available'" class="font-mono">{{ entry.egress.ip }}</span>
                <button v-else-if="entry.egress.state === 'failed'" type="button" class="rounded font-medium text-red-600 underline decoration-dotted underline-offset-2 hover:text-red-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 dark:text-red-400 dark:hover:text-red-300" :title="t('admin.accounts.openai.codexTicketLogs.egressViewFailure')" @click="openEgressFailure(entry, $event)">{{ t('admin.accounts.openai.codexTicketLogs.egressFailed') }}</button>
                <span v-else-if="entry.egress.state === 'probing'" class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.codexTicketLogs.egressProbing') }}</span>
                <span v-else :title="t('admin.accounts.openai.codexTicketLogs.egressNotAttemptedHint')">—</span>
              </td>
              <td class="whitespace-nowrap px-3 py-2.5">
                <span v-if="entry.egress.country" class="inline-flex items-center gap-1.5">
                  <img :src="`https://unpkg.com/flag-icons/flags/4x3/${entry.egress.country.code.toLowerCase()}.svg`" alt="" aria-hidden="true" class="h-3 w-4 shrink-0 rounded-sm object-cover" width="16" height="12" loading="lazy" referrerpolicy="no-referrer" />
                  <span>{{ entry.egress.country.name }}</span>
                </span>
                <span v-else :title="t(entry.egress.ip ? 'admin.accounts.openai.codexTicketLogs.egressCountryPendingHint' : 'admin.accounts.openai.codexTicketLogs.egressUnavailableHint')">—</span>
              </td>
              <td class="whitespace-nowrap px-3 py-2.5 tabular-nums">{{ entry.ticket_length ?? '—' }} / {{ entry.target_length }}</td>
              <td class="whitespace-nowrap px-3 py-2.5 tabular-nums">{{ entry.duration_ms === undefined ? '—' : `${entry.duration_ms} ms` }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="space-y-1 text-xs text-gray-500 dark:text-gray-400">
        <p v-if="lastUpdated">{{ t('admin.accounts.openai.codexTicketLogs.updated', { time: formatTime(lastUpdated) }) }}</p>
        <p>{{ response ? t('admin.accounts.openai.codexTicketLogs.retention', { count: response.limit }) : t('admin.accounts.openai.codexTicketLogs.retentionPending') }}</p>
      </div>
    </div>
    <template #footer>
      <div class="flex justify-end gap-2">
        <button type="button" class="btn btn-secondary" :disabled="initialLoading" @click="refresh">{{ t('common.refresh') }}</button>
        <button type="button" class="btn btn-primary" @click="close">{{ t('common.close') }}</button>
      </div>
    </template>
  </BaseDialog>
  <BaseDialog v-if="egressFailure" :show="show" :title="t('admin.accounts.openai.codexTicketLogs.egressFailureTitle')" width="normal" :z-index="60" :close-on-escape="false" @close="closeEgressFailure()">
    <div class="space-y-4">
      <div class="space-y-1 rounded-xl bg-gray-50 p-4 text-sm dark:bg-dark-800">
        <p class="break-words font-medium text-gray-900 dark:text-gray-100">{{ egressFailure.accountName }} <span class="font-normal text-gray-500">#{{ egressFailure.accountId }}</span></p>
        <p class="break-all font-mono text-xs text-gray-600 dark:text-gray-400">{{ egressFailure.model }}</p>
        <dl class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 pt-2 text-xs text-gray-600 dark:text-gray-400">
          <dt>{{ t('admin.accounts.openai.codexTicketLogs.attempt') }}</dt><dd class="tabular-nums">{{ egressFailure.attempt }}</dd>
          <dt>{{ t('admin.accounts.openai.codexTicketLogs.egressRecordTime') }}</dt><dd class="tabular-nums">{{ formatTime(egressFailure.time) }}</dd>
        </dl>
      </div>
      <div class="space-y-2 text-sm">
        <p class="font-medium text-gray-900 dark:text-gray-100">{{ t('admin.accounts.openai.codexTicketLogs.reason') }}</p>
        <p class="text-gray-700 dark:text-gray-300">{{ egressFailureReason }}</p>
      </div>
      <dl v-if="egressFailure.httpStatus !== undefined" class="flex flex-wrap gap-x-3 gap-y-1 text-sm">
        <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.codexTicketLogs.egressProbeStatus') }}</dt>
        <dd class="font-mono text-red-600 dark:text-red-400">{{ egressFailure.httpStatus }}</dd>
      </dl>
    </div>
    <template #footer>
      <div class="flex justify-end"><button type="button" class="btn btn-primary" @click="closeEgressFailure()">{{ t('common.close') }}</button></div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { isCodexTicketRateLimited } from '@/utils/codexTicketStatus'
import type { Account, CodexTicketLogEntry, CodexTicketLogsResponse } from '@/types'

interface EgressResult {
  state: 'available' | 'failed' | 'probing' | 'not_attempted'
  ip?: string
  country: { code: string; name: string } | null
  error?: CodexTicketLogEntry['egress_error']
}

interface EgressFailureSnapshot {
  accountId: number
  accountName: string
  model: string
  attempt: number
  time: string
  reason?: string
  httpStatus?: number
}

const props = defineProps<{ show: boolean; account: Account; model: string }>()
const emit = defineEmits<{ close: [] }>()
const { t, locale } = useI18n()
const response = ref<CodexTicketLogsResponse | null>(null)
const refreshing = ref(false)
// Keep background polling silent once a snapshot is visible.
const initialLoading = computed(() => !response.value && refreshing.value)
const failed = ref(false)
const lastUpdated = ref<string | null>(null)
const egressFailure = ref<EgressFailureSnapshot | null>(null)
let egressFailureTrigger: HTMLElement | null = null
let disposed = false
const regionNames = computed(() => new Intl.DisplayNames([locale.value.startsWith('zh') ? 'zh-CN' : 'en'], { type: 'region' }))
const entries = computed(() => {
  const completed = new Map<number, CodexTicketLogEntry>()
  return [...(response.value?.entries ?? [])].sort((a, b) => b.id - a.id).map((entry) => {
    let result = entry
    if (entry.event === 'started') {
      // Pair each start with its following result, consuming it so repeated
      // attempt numbers from a later round do not leak into an earlier round.
      if (!entry.egress_ip && !entry.egress_error) result = completed.get(entry.attempt) ?? entry
      completed.delete(entry.attempt)
    } else {
      completed.set(entry.attempt, entry)
    }
    return { ...entry, egress: egressResult(result) }
  })
})
const status = computed(() => response.value
  ? response.value.status
  : props.account.codex_turn_tickets?.find((ticket) => ticket.model === props.model) ?? null)
const now = ref(Date.now())
const { pause: pauseClock, resume: resumeClock } = useIntervalFn(
  () => { now.value = Date.now() },
  1000,
  { immediate: false }
)
const rateLimited = computed(() => isCodexTicketRateLimited(props.account, props.model, now.value))
const statusLabel = computed(() => {
  if (status.value?.token_invalid) return t('admin.accounts.openai.codexTurnTicketTokenInvalid')
  if (rateLimited.value) return t('admin.accounts.openai.codexTurnTicketRateLimited')
  if (status.value?.ready) return t('admin.accounts.openai.codexTicketLogs.ready')
  if (status.value?.blocked) return t('admin.accounts.openai.codexTurnTicketPaused')
  return t('admin.accounts.openai.codexTurnTicketMissing')
})

const knownEvents = new Set(['started', 'success', 'miss', 'error', 'skipped'])
const knownReasons = new Set([
  'request_started', 'harvested', 'http_error', 'quota_exhausted', 'missing_state', 'length_mismatch',
  'invalid_state', 'token_error', 'token_invalid', 'timeout', 'canceled', 'network_error', 'request_error'
])
const knownEgressReasons = new Set([
  'timeout', 'canceled', 'network_error', 'http_error', 'connection_closed', 'unsupported_protocol',
  'response_too_large', 'response_read_error', 'missing_ip', 'invalid_ip', 'ambiguous_ip',
  'connection_changed', 'ticket_connection_unavailable', 'insufficient_time', 'unsupported_transport',
  'request_error', 'unknown'
])
const egressFailureReason = computed(() => {
  const reason = egressFailure.value?.reason
  if (!reason) return t('admin.accounts.openai.codexTicketLogs.egressMissingDiagnostic')
  return t(`admin.accounts.openai.codexTicketLogs.egressReasons.${knownEgressReasons.has(reason) ? reason : 'unknown'}`)
})

// egressResult 解析出口诊断结果并区分已确认地址和检测失败
function egressResult(entry: CodexTicketLogEntry): EgressResult {
  if (entry.egress_ip) return { state: 'available', ip: entry.egress_ip, country: countryDetails(entry.egress_country_code) }
  if (entry.attempt <= 0 || entry.event === 'skipped' || entry.reason === 'token_error') return { state: 'not_attempted', country: null }
  if (entry.egress_error) return { state: 'failed', error: entry.egress_error, country: null }
  return { state: entry.event === 'started' ? 'probing' : 'failed', country: null }
}

// openEgressFailure 展示出口检测失败详情并管理弹窗焦点
function openEgressFailure(entry: CodexTicketLogEntry & { egress: EgressResult }, event: MouseEvent) {
  egressFailureTrigger = event.currentTarget as HTMLElement
  // Preserve the selected record even if polling updates or evicts its row.
  egressFailure.value = {
    accountId: props.account.id, accountName: props.account.name, model: props.model,
    attempt: entry.attempt, time: entry.time,
    reason: entry.egress.error?.reason, httpStatus: entry.egress.error?.http_status
  }
}

// closeEgressFailure 关闭出口失败详情并按需恢复焦点
async function closeEgressFailure(restoreFocus = true) {
  if (!egressFailure.value) return
  const trigger = egressFailureTrigger
  egressFailure.value = null
  egressFailureTrigger = null
  await nextTick()
  // BaseDialog releases the shared body lock when the upper dialog unmounts.
  // Keep the still-open log dialog locked without resetting its scroll position.
  if (props.show && active && !disposed) {
    document.body.classList.add('modal-open')
    if (restoreFocus && trigger?.isConnected) trigger.focus({ preventScroll: true })
  }
}

// handleDialogEscape 优先关闭当前最上层的日志详情弹窗
function handleDialogEscape(event: KeyboardEvent) {
  if (event.key !== 'Escape' || !props.show) return
  event.preventDefault()
  event.stopImmediatePropagation()
  if (egressFailure.value) void closeEgressFailure()
  else close()
}

// Restrict flag URLs to assigned ISO 3166-1 alpha-2 regions; Intl also knows
// special regions such as ZZ (unknown) that should not produce a flag.
const countryCodes = new Set((
  'AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ ' +
  'BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ ' +
  'CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ ' +
  'DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR ' +
  'GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY ' +
  'HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP ' +
  'KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY ' +
  'MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ ' +
  'NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY ' +
  'QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ ' +
  'TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ ' +
  'VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW'
).split(' '))

// countryDetails 解析国家代码对应的本地化名称与图标
function countryDetails(value?: string) {
  const code = value?.toUpperCase()
  if (!code || !countryCodes.has(code)) return null
  return { code, name: regionNames.value.of(code) || code }
}

// eventLabel 返回打票事件的本地化显示文本
function eventLabel(event: string) {
  return t(`admin.accounts.openai.codexTicketLogs.events.${knownEvents.has(event) ? event : 'unknown'}`)
}

// reasonLabel 返回打票结果原因的本地化显示文本
function reasonLabel(reason: string) {
  return t(`admin.accounts.openai.codexTicketLogs.reasons.${knownReasons.has(reason) ? reason : 'unknown'}`)
}

// eventClass 根据事件类型选择日志状态样式
function eventClass(event: string) {
  if (event === 'success') return 'text-emerald-600 dark:text-emerald-400'
  if (event === 'error') return 'text-red-600 dark:text-red-400'
  if (event === 'miss') return 'text-amber-600 dark:text-amber-400'
  if (event === 'started') return 'text-blue-600 dark:text-blue-400'
  return 'text-gray-500 dark:text-gray-400'
}

// formatTime 格式化日志时间以供本地显示
function formatTime(value: string) {
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return '—'
  return date.toLocaleString(locale.value, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })
}

let timer: ReturnType<typeof setTimeout> | undefined
let controller: AbortController | null = null
let generation = 0
let active = false

// stop 停止日志轮询并取消尚未完成的请求
function stop() {
  active = false
  pauseClock()
  generation += 1
  clearTimeout(timer)
  timer = undefined
  controller?.abort()
  controller = null
  refreshing.value = false
}

// refresh 刷新日志快照并丢弃账号或模型切换前的旧响应
async function refresh() {
  if (!active || refreshing.value) return
  clearTimeout(timer)
  timer = undefined
  const currentGeneration = generation
  const requestController = new AbortController()
  controller = requestController
  refreshing.value = true
  try {
    const result = await adminAPI.accounts.getCodexTicketLogs(props.account.id, props.model, { signal: requestController.signal })
    if (currentGeneration !== generation || requestController.signal.aborted) return
    if (result.model !== props.model) throw new Error('Unexpected ticket log model')
    response.value = result
    failed.value = false
    lastUpdated.value = new Date().toISOString()
  } catch {
    if (currentGeneration === generation && !requestController.signal.aborted) failed.value = true
  } finally {
    if (currentGeneration === generation) {
      controller = null
      refreshing.value = false
      if (active) timer = setTimeout(() => { void refresh() }, 2000)
    }
  }
}

// close 关闭日志弹窗并清理轮询状态
function close() {
  stop()
  void closeEgressFailure(false)
  emit('close')
}

// The account list replaces snapshots while polling; only an actual identity
// change should clear the log table and restart the request lifecycle.
watch([() => props.show, () => props.account.id, () => props.model], () => {
  stop()
  void closeEgressFailure(false)
  response.value = null
  failed.value = false
  lastUpdated.value = null
  active = props.show && Boolean(props.model)
  if (active) {
    now.value = Date.now()
    resumeClock()
    void refresh()
  }
}, { immediate: true })

onMounted(() => document.addEventListener('keydown', handleDialogEscape, true))
onBeforeUnmount(() => {
  disposed = true
  document.removeEventListener('keydown', handleDialogEscape, true)
  stop()
})
</script>
