<template>
  <section class="card p-4">
    <h3 class="font-semibold text-gray-900 dark:text-white">{{ t('admin.groups.capacityMonitor.forecast') }}</h3>
    <p class="mt-1 text-xs text-gray-500">{{ t('admin.groups.capacityMonitor.cumulativeHint') }}</p>
    <div v-if="points.length && points.some(p => p.capacity != null)" class="mt-4 h-72 sm:h-80">
      <Line :data="chartData" :options="options" :aria-label="t('admin.groups.capacityMonitor.forecast')" role="img" />
    </div>
    <p v-else class="py-12 text-center text-sm text-gray-500">{{ t('admin.groups.capacityMonitor.noCapacity') }}</p>
    <template v-if="points.length">
      <label class="mt-3 block text-xs text-gray-500" :for="`capacity-time-${id}`">{{ t('admin.groups.capacityMonitor.inspectTime') }}</label>
      <input :id="`capacity-time-${id}`" v-model.number="selectedIndex" class="mt-1 w-full accent-primary-600" type="range" min="0" :max="points.length - 1" step="1" />
      <p v-if="selected" class="mt-1 text-xs leading-6 text-gray-600 dark:text-gray-300" aria-live="polite">
        {{ dateTime(selected.at, timezone, locale) }} · {{ t('admin.groups.capacityMonitor.afterHours', { hours: selected.hours.toFixed(1) }) }}
        · {{ t('admin.groups.capacityMonitor.capacity') }} {{ usd(selected.capacity, locale) }}
        · {{ t('admin.groups.capacityMonitor.demand') }} {{ usd(selectedDemand, locale) }}
        · {{ t('admin.groups.capacityMonitor.gap') }} {{ usd(selectedGap, locale) }}
        · {{ t('admin.groups.capacityMonitor.direct') }} {{ usd(selected.direct_capacity, locale) }}
        / {{ t('admin.groups.capacityMonitor.estimated') }} {{ usd(selected.estimated_capacity, locale) }}
        · {{ t('admin.groups.capacityMonitor.modeledAccounts', { count: selected.account_count }) }}
      </p>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Chart as ChartJS, LinearScale, PointElement, LineElement, Tooltip, Legend, Filler, type ChartOptions } from 'chart.js'
import { Line } from 'vue-chartjs'
import type { CapacityPoint } from '@/api/admin/groupCapacityMonitor'
import { dateTime, usd } from './monitorFormat'

ChartJS.register(LinearScale, PointElement, LineElement, Tooltip, Legend, Filler)
const props = defineProps<{ id: number; points: CapacityPoint[]; rate: number | null; maxHours: number; timezone: string; focusHours?: number; focusVersion?: number }>()
const { t, locale } = useI18n()
const points = computed(() => props.points.filter(p => p.hours <= props.maxHours))
const selectedIndex = ref(0)
const selected = computed(() => points.value[selectedIndex.value])
const demand = (hours: number) => props.rate == null ? null : props.rate * hours
const selectedDemand = computed(() => selected.value ? demand(selected.value.hours) : null)
const selectedGap = computed(() => selected.value?.capacity != null && selectedDemand.value != null ? selected.value.capacity - selectedDemand.value : null)
watch(() => [props.focusHours, props.focusVersion, points.value] as const, () => {
  const requested = props.focusHours ?? 5
  const index = points.value.findIndex(p => p.hours >= requested)
  selectedIndex.value = index < 0 ? Math.max(0, points.value.length - 1) : index
}, { immediate: true })

const chartData = computed(() => ({ datasets: [
  { label: t('admin.groups.capacityMonitor.capacity'), data: points.value.map(p => ({ x: p.hours, y: p.capacity })),
    // Chart.js 'before' holds the previous value until the target timestamp.
    borderColor: '#3b82f6', borderWidth: 2, pointRadius: 0, pointHitRadius: 12, stepped: 'before' as const,
    fill: props.rate == null ? false : { target: 1, above: 'rgba(59,130,246,.10)', below: 'rgba(239,68,68,.15)' } },
  { label: t('admin.groups.capacityMonitor.demand'), data: points.value.map(p => ({ x: p.hours, y: demand(p.hours) })),
    borderColor: '#f59e0b', borderWidth: 2, borderDash: [6, 4], pointRadius: 0, pointHitRadius: 12, fill: false },
] }))
const options = computed<ChartOptions<'line'>>(() => ({
  responsive: true, maintainAspectRatio: false, animation: false,
  interaction: { mode: 'index', intersect: false },
  plugins: { legend: { position: 'bottom', labels: { color: '#94a3b8', usePointStyle: true } }, tooltip: { callbacks: {
    title: items => {
      const p = points.value[items[0]?.dataIndex ?? -1]
      return p ? `${dateTime(p.at, props.timezone, locale.value)} (+${p.hours.toFixed(1)}h)` : ''
    },
    label: item => `${item.dataset.label}: ${usd(item.parsed.y, locale.value)}`,
    footer: items => {
      const p = points.value[items[0]?.dataIndex ?? -1]
      if (!p) return ''
      const d = demand(p.hours)
      return [
        `${t('admin.groups.capacityMonitor.gap')}: ${usd(p.capacity != null && d != null ? p.capacity - d : null, locale.value)}`,
        `${t('admin.groups.capacityMonitor.direct')}: ${usd(p.direct_capacity, locale.value)} / ${t('admin.groups.capacityMonitor.estimated')}: ${usd(p.estimated_capacity, locale.value)}`,
        t('admin.groups.capacityMonitor.modeledAccounts', { count: p.account_count }),
      ]
    },
  } } },
  scales: {
    x: { type: 'linear', min: 0, max: props.maxHours, grid: { display: false }, ticks: { color: '#94a3b8', maxTicksLimit: 8, callback: v => `${Number(v)}h` } },
    y: { beginAtZero: true, title: { display: true, text: 'USD', color: '#94a3b8' }, ticks: { color: '#94a3b8', callback: v => `$${Number(v).toLocaleString(locale.value)}` }, grid: { color: 'rgba(148,163,184,.15)' } },
  },
}))
</script>
