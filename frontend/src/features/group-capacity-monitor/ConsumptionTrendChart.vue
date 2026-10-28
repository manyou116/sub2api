<template>
  <section class="card p-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h3 class="font-semibold text-gray-900 dark:text-white">{{ t('admin.groups.capacityMonitor.history') }}</h3>
      <div class="flex gap-1" role="group" :aria-label="t('admin.groups.capacityMonitor.historyRange')">
        <button v-for="range in ranges" :key="range" type="button" :class="['btn btn-sm', selectedRange === range ? 'btn-primary' : 'btn-secondary']" :aria-pressed="selectedRange === range" @click="selectedRange = range">{{ range }}</button>
      </div>
    </div>
    <p class="mt-1 text-xs text-gray-500">{{ t('admin.groups.capacityMonitor.historyHint') }}</p>
    <div v-if="visible.length" class="mt-4 h-64 sm:h-72">
      <Line :data="chartData" :options="options" :aria-label="t('admin.groups.capacityMonitor.history')" role="img" />
    </div>
    <p v-else class="py-10 text-center text-sm text-gray-500">{{ t('admin.groups.capacityMonitor.noHistory') }}</p>
    <p class="mt-3 text-xs leading-6 text-gray-500">
      {{ t('admin.groups.capacityMonitor.full24h') }}: {{ usd(summary.recentCost, locale) }}
      · {{ t('admin.groups.capacityMonitor.change') }}: {{ summary.change == null ? '—' : `${summary.change > 0 ? '+' : ''}${summary.change.toFixed(1)}%` }}
      · {{ t('admin.groups.capacityMonitor.peak') }}: {{ usd(visibleSummary.peak?.pool_account_cost, locale) }}
      <template v-if="visibleSummary.peak"> ({{ dateTime(visibleSummary.peak.start_at, timezone, locale) }})</template>
    </p>
    <template v-if="visible.length">
      <label :for="`usage-time-${asOf}`" class="mt-2 block text-xs text-gray-500">{{ t('admin.groups.capacityMonitor.inspectHour') }}</label>
      <input :id="`usage-time-${asOf}`" v-model.number="selectedIndex" class="mt-1 w-full accent-primary-600" type="range" min="0" :max="visible.length - 1" step="1" />
      <p v-if="selected" class="text-xs leading-6 text-gray-600 dark:text-gray-300" aria-live="polite">
        {{ dateTime(selected.start_at, timezone, locale) }} – {{ dateTime(selected.end_at, timezone, locale) }}
        <strong v-if="!selected.complete" class="text-amber-600 dark:text-amber-400"> · {{ t('admin.groups.capacityMonitor.partialHour') }}</strong>
        · {{ t('admin.groups.capacityMonitor.pool') }} {{ usd(selected.pool_account_cost, locale) }}
        · {{ t('admin.groups.capacityMonitor.route') }} {{ usd(selected.group_route_cost, locale) }}
      </p>
    </template>
    <p class="text-xs text-amber-600 dark:text-amber-400">{{ t('admin.groups.capacityMonitor.historyUnknown') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Chart as ChartJS, CategoryScale, LinearScale, PointElement, LineElement, Tooltip, Legend, type ChartOptions } from 'chart.js'
import { Line } from 'vue-chartjs'
import type { CapacityHistoryPoint, MonitorRange } from '@/api/admin/groupCapacityMonitor'
import { dateTime, historySummary, rangeHours, usd } from './monitorFormat'

ChartJS.register(CategoryScale, LinearScale, PointElement, LineElement, Tooltip, Legend)
const props = defineProps<{ points: CapacityHistoryPoint[]; timezone: string; asOf: string }>()
const { t, locale } = useI18n()
const ranges: MonitorRange[] = ['24h', '7d', '15d']
const selectedRange = ref<MonitorRange>('7d')
const summary = computed(() => historySummary(props.points))
const visible = computed(() => props.points.filter(p => Date.parse(p.start_at) >= (summary.value.end || Date.parse(props.asOf)) - rangeHours[selectedRange.value] * 3_600_000))
const visibleSummary = computed(() => historySummary(visible.value))
const selectedIndex = ref(0)
const selected = computed(() => visible.value[selectedIndex.value])
watch(visible, points => { selectedIndex.value = Math.max(0, points.length - 1) }, { immediate: true })
const chartData = computed(() => ({
  labels: visible.value.map(p => dateTime(p.start_at, props.timezone, locale.value)),
  datasets: [
    { label: t('admin.groups.capacityMonitor.pool'), data: visible.value.map(p => p.pool_account_cost), borderColor: '#3b82f6', borderWidth: 2,
      pointRadius: visible.value.map(p => p.complete ? 0 : 4), pointBackgroundColor: '#94a3b8', stepped: 'before' as const },
    { label: t('admin.groups.capacityMonitor.route'), data: visible.value.map(p => p.group_route_cost), borderColor: '#8b5cf6', borderWidth: 2,
      pointRadius: 0, borderDash: [5, 4], stepped: 'before' as const },
  ],
}))
const options = computed<ChartOptions<'line'>>(() => ({
  responsive: true, maintainAspectRatio: false, animation: false, interaction: { mode: 'index', intersect: false },
  plugins: { legend: { position: 'bottom', labels: { color: '#94a3b8', usePointStyle: true } }, tooltip: { callbacks: {
    title: items => {
      const p = visible.value[items[0]?.dataIndex ?? -1]
      return p ? `${dateTime(p.start_at, props.timezone, locale.value)} – ${dateTime(p.end_at, props.timezone, locale.value)}${p.complete ? '' : ` · ${t('admin.groups.capacityMonitor.partialHour')}`}` : ''
    },
    label: item => `${item.dataset.label}: ${usd(item.parsed.y, locale.value)}`,
    footer: items => {
      const p = visible.value[items[0]?.dataIndex ?? -1]
      return p ? [t('admin.groups.capacityMonitor.requests', { pool: p.pool_requests, route: p.group_route_requests }), `${t('admin.groups.capacityMonitor.otherGroups')}: ${usd(p.pool_other_cost, locale.value)}`] : ''
    },
  } } },
  scales: {
    x: { ticks: { color: '#94a3b8', maxTicksLimit: 8, maxRotation: 0 }, grid: { display: false } },
    y: { beginAtZero: true, title: { display: true, text: t('admin.groups.capacityMonitor.hourlyUsd'), color: '#94a3b8' }, ticks: { color: '#94a3b8', callback: v => `$${Number(v).toLocaleString(locale.value)}` }, grid: { color: 'rgba(148,163,184,.15)' } },
  },
}))
</script>
