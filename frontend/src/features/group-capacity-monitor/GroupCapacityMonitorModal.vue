<template>
  <BaseDialog :show="show" :title="`${group?.name || ''} · ${t('admin.groups.capacityMonitor.title')}`" width="full" @close="emit('close')">
    <div class="space-y-5" data-testid="capacity-monitor">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div class="space-y-1 text-sm text-gray-600 dark:text-gray-300">
          <p class="font-medium">{{ t('admin.groups.capacityMonitor.scope') }} · {{ t('admin.groups.capacityMonitor.moneyLabel') }}</p>
          <p class="text-xs text-gray-500">{{ t('admin.groups.capacityMonitor.moneyHint') }}</p>
          <p v-if="data" class="text-xs">{{ t('admin.groups.capacityMonitor.updated') }} {{ dateTime(data.as_of, data.timezone, locale) }} ({{ data.timezone }})</p>
        </div>
        <button type="button" class="btn btn-secondary" :disabled="loading" @click="load">{{ loading ? t('common.loading') : t('admin.groups.capacityMonitor.refresh') }}</button>
      </div>
      <div v-if="error" role="alert" class="rounded-xl bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">
        {{ t('admin.groups.capacityMonitor.loadFailed') }} {{ error }}
      </div>
      <div v-if="loading && !data" class="py-16 text-center"><LoadingSpinner /></div>
      <template v-if="data">
        <div v-if="stale" class="rounded-xl bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-900/20 dark:text-amber-300" role="status">{{ t('admin.groups.capacityMonitor.stale') }}</div>
        <div class="rounded-xl bg-gray-50 p-3 text-xs leading-6 text-gray-600 dark:bg-dark-800 dark:text-gray-300">
          <p>{{ t('admin.groups.capacityMonitor.coverage', { modeled: data.inventory.modeled_count, eligible: data.inventory.eligible_count, total: data.inventory.member_count, schedulable: data.inventory.schedulable_count }) }}</p>
          <p>{{ t('admin.groups.capacityMonitor.provenanceHint') }}</p>
          <p>{{ t('admin.groups.capacityMonitor.sampleRange') }}: {{ dateTime(data.quality.oldest_sample_at, data.timezone, locale) }} – {{ dateTime(data.quality.freshest_sample_at, data.timezone, locale) }}</p>
        </div>
        <div class="grid gap-3 sm:grid-cols-3">
          <div v-for="card in cards" :key="card.label" class="card p-4">
            <p class="text-xs text-gray-500">{{ card.label }}</p><p class="mt-2 text-2xl font-semibold tabular-nums text-gray-900 dark:text-white">{{ card.value }}</p>
          </div>
        </div>
        <div :class="['rounded-xl border p-3 text-sm', hasShortfall ? 'border-amber-300 bg-amber-50 text-amber-900 dark:border-amber-800 dark:bg-amber-900/20 dark:text-amber-200' : 'border-gray-200 text-gray-600 dark:border-dark-600 dark:text-gray-300']" role="status" data-testid="capacity-pressure">
          <p class="font-medium">{{ pressureText }}</p>
          <p class="mt-1 text-xs">{{ t('admin.groups.capacityMonitor.pressureHint') }}</p>
        </div>
        <div class="flex flex-wrap items-center justify-between gap-3">
          <label class="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-300">
            {{ t('admin.groups.capacityMonitor.demandBasis') }}
            <select v-model="basisKey" class="input w-auto" data-testid="capacity-basis">
              <option v-for="basis in data.demand_bases" :key="basis.key" :value="basis.key">{{ basisLabel(basis.key) }}</option>
            </select>
          </label>
          <div class="flex items-center gap-2 text-sm text-gray-500" role="group" :aria-label="t('admin.groups.capacityMonitor.chartRange')">
            {{ t('admin.groups.capacityMonitor.chartRange') }}
            <button v-for="range in ranges" :key="range" type="button" :aria-pressed="forecastRange === range" :class="['btn btn-sm', forecastRange === range ? 'btn-primary' : 'btn-secondary']" @click="forecastRange = range">{{ range }}</button>
          </div>
        </div>
        <p v-if="!basis?.available" class="text-sm text-amber-600 dark:text-amber-400">{{ t('admin.groups.capacityMonitor.noDemand') }}</p>
        <p v-else-if="basis.reason" class="text-xs text-amber-600 dark:text-amber-400">{{ reasonLabel(basis.reason) }}</p>
        <CapacityForecastChart :id="group?.id || 0" :points="data.capacity_points" :rate="basis?.available ? basis.rate_per_hour : null" :max-hours="rangeHours[forecastRange]" :timezone="data.timezone" :focus-hours="focusHours" :focus-version="focusVersion" />
        <section class="card overflow-hidden">
          <h3 class="p-4 font-semibold text-gray-900 dark:text-white">{{ t('admin.groups.capacityMonitor.milestones') }}</h3>
          <div class="overflow-x-auto">
            <table class="w-full whitespace-nowrap text-left text-sm">
              <thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-800"><tr><th v-for="key in tableColumns" :key="key" class="px-4 py-3 font-medium">{{ t(`admin.groups.capacityMonitor.${key}`) }}</th></tr></thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-for="row in scenario?.milestones || []" :key="row.hours">
                  <td class="px-4 py-3"><button type="button" class="text-primary-600 underline decoration-dotted underline-offset-4 dark:text-primary-400" @click="focusMilestone(row.hours)">{{ horizonLabel(row.hours) }}</button></td>
                  <td class="px-4 py-3 tabular-nums">{{ usd(row.capacity, locale) }}</td><td class="px-4 py-3 tabular-nums">{{ usd(row.demand, locale) }}</td>
                  <td :class="['px-4 py-3 tabular-nums', row.gap != null && row.gap < 0 ? 'text-red-600 dark:text-red-400' : '']">{{ usd(row.gap, locale) }}</td>
                  <td class="px-4 py-3 text-xs text-gray-500">{{ t('admin.groups.capacityMonitor.tableCoverage', { modeled: data.inventory.modeled_count, eligible: data.inventory.eligible_count }) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
        <ConsumptionTrendChart :points="data.history" :timezone="data.timezone" :as-of="data.as_of" />
        <details class="card p-4">
          <summary class="cursor-pointer text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.groups.capacityMonitor.details') }}</summary>
          <dl class="mt-4 grid grid-cols-2 gap-3 text-xs sm:grid-cols-3 lg:grid-cols-4">
            <div v-for="item in inventoryItems" :key="item.key"><dt class="text-gray-500">{{ t(`admin.groups.capacityMonitor.inventory.${item.key}`) }}</dt><dd class="mt-1 font-medium tabular-nums">{{ item.value }}</dd></div>
          </dl>
          <p class="mt-4 text-xs text-gray-500">{{ t('admin.groups.capacityMonitor.countsOverlap') }}</p>
          <p class="mt-2 text-xs text-gray-500">{{ t('admin.groups.capacityMonitor.unclassified') }}: {{ usd(data.quality.unclassified_cost, locale) }}</p>
          <h4 class="mt-5 text-sm font-medium">{{ t('admin.groups.capacityMonitor.sharedUsage') }}</h4>
          <ul class="mt-2 space-y-1 text-xs text-gray-500">
            <li v-for="item in data.shared_usage" :key="item.group_id ?? 'none'">{{ item.group_name || (item.group_id == null ? '—' : `#${item.group_id}`) }}: {{ usd(item.cost, locale) }}</li>
          </ul>
          <p class="mt-3 text-xs text-gray-500">{{ t('admin.groups.capacityMonitor.sharedHint') }}</p>
          <p class="mt-3 text-xs text-gray-500">{{ t('admin.groups.capacityMonitor.limitationsHint') }}</p>
          <ul class="mt-2 list-disc space-y-1 pl-5 text-xs text-gray-500"><li v-for="reason in data.limitations" :key="reason">{{ reasonLabel(reason) }}</li></ul>
        </details>
      </template>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import { extractApiErrorMessage } from '@/utils/apiError'
import { getGroupCapacityMonitor, type DemandBasisKey, type GroupCapacityMonitor, type MonitorRange } from '@/api/admin/groupCapacityMonitor'
import CapacityForecastChart from './CapacityForecastChart.vue'
import ConsumptionTrendChart from './ConsumptionTrendChart.vue'
import { dateTime, rangeHours, usd } from './monitorFormat'

const props = defineProps<{ show: boolean; group: { id: number; name: string } | null }>()
const emit = defineEmits<{ close: [] }>()
const { t, te, locale } = useI18n()
const data = ref<GroupCapacityMonitor | null>(null)
const loading = ref(false)
const error = ref('')
const now = ref(Date.now())
const basisKey = ref<DemandBasisKey>('last24h')
const forecastRange = ref<MonitorRange>('15d')
const focusHours = ref(5)
const focusVersion = ref(0)
const ranges: MonitorRange[] = ['24h', '7d', '15d']
const tableColumns = ['period', 'capacity', 'demand', 'gap', 'coverageLabel']
let controller: AbortController | null = null
let clock: ReturnType<typeof setInterval> | undefined
let loadedGroup: number | null = null
const basis = computed(() => data.value?.demand_bases.find(b => b.key === basisKey.value))
const scenario = computed(() => data.value?.scenarios.find(s => s.key === basisKey.value))
const stale = computed(() => !!data.value && (!!error.value || (Date.parse(data.value.cache.expires_at) || 0) < now.value))
const hasShortfall = computed(() => !!scenario.value?.first_shortfall_at && !!data.value && Date.parse(scenario.value.first_shortfall_at) < Date.parse(data.value.as_of) + rangeHours[forecastRange.value] * 3_600_000)
const pressureText = computed(() => {
  if (data.value && !data.value.inventory.eligible_count) return t('admin.groups.capacityMonitor.unsupported')
  if (!data.value?.inventory.modeled_count) return t('admin.groups.capacityMonitor.noCapacity')
  if (!basis.value?.available || basis.value.rate_per_hour == null) return t('admin.groups.capacityMonitor.noDemand')
  if (hasShortfall.value) return t('admin.groups.capacityMonitor.pressure', { time: dateTime(scenario.value?.first_shortfall_at, data.value.timezone, locale.value) })
  return t('admin.groups.capacityMonitor.noShortfall')
})
const cards = computed(() => {
  const milestones = scenario.value?.milestones || []
  const recent = data.value?.demand_bases.find(b => b.key === 'last24h')
  return [
    { label: t('admin.groups.capacityMonitor.next5h'), value: usd(milestones.find(p => p.hours === 5)?.capacity, locale.value) },
    { label: t('admin.groups.capacityMonitor.next24hGap'), value: usd(milestones.find(p => p.hours === 24)?.gap, locale.value) },
    { label: t('admin.groups.capacityMonitor.recentRate'), value: `${usd(recent?.available ? recent.rate_per_hour : null, locale.value)}/h` },
  ]
})
const inventoryItems = computed(() => {
  const keys = ['direct_count', 'estimated_count', 'history_account_count', 'stale_count', 'missing_window_count', 'shared_count', 'unknown_identity_count', 'provenance_unknown_count', 'state_excluded_count', 'duplicate_count']
  return keys.filter(key => data.value?.inventory[key] != null).map(key => ({ key, value: data.value!.inventory[key] }))
})
function basisLabel(key: DemandBasisKey) { return t(`admin.groups.capacityMonitor.bases.${key}`) }
function reasonLabel(reason: string) {
  const key = `admin.groups.capacityMonitor.reasons.${reason}`
  return te(key) ? t(key) : t('admin.groups.capacityMonitor.additionalLimitation')
}
function horizonLabel(hours: number) { return hours < 24 ? `${hours}h` : `${hours / 24}d` }
function focusMilestone(hours: number) {
  forecastRange.value = hours <= 24 ? '24h' : hours <= 168 ? '7d' : '15d'
  focusHours.value = hours
  focusVersion.value++
}
async function load() {
  if (!props.show || !props.group) return
  controller?.abort()
  const request = new AbortController()
  controller = request
  loading.value = true
  error.value = ''
  try {
    const result = await getGroupCapacityMonitor(props.group.id, request.signal)
    if (!request.signal.aborted) { data.value = result; now.value = Date.now() }
  } catch (err) {
    if (!request.signal.aborted) error.value = extractApiErrorMessage(err, t('admin.groups.capacityMonitor.loadFailed'))
  } finally {
    if (controller === request) loading.value = false
  }
}
watch(() => [props.show, props.group?.id] as const, ([show, id]) => {
  controller?.abort()
  if (clock) clearInterval(clock)
  if (!show || id == null) return
  if (loadedGroup !== id) { data.value = null; loadedGroup = id; basisKey.value = 'last24h'; forecastRange.value = '15d' }
  now.value = Date.now()
  clock = setInterval(() => { now.value = Date.now() }, 30_000)
  void load()
}, { immediate: true })
onUnmounted(() => { controller?.abort(); if (clock) clearInterval(clock) })
</script>
