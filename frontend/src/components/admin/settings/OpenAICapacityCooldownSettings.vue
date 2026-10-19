<template>
  <section ref="card" class="card" data-testid="capacity-cooldown-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t(`${prefix}.title`) }}</h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t(`${prefix}.description`) }}</p>
    </div>
    <div class="space-y-5 p-6">
      <p v-if="loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <div v-else-if="loadFailed" role="alert" class="text-sm text-red-600">
        {{ t(`${prefix}.loadFailed`) }}
        <button type="button" class="btn btn-secondary btn-sm ml-3" @click="load">{{ t('common.tryAgain') }}</button>
      </div>
      <fieldset :disabled="loading || loadFailed || saving" class="space-y-5">
        <div class="flex items-center justify-between gap-4">
          <label for="capacity-enabled" class="text-sm font-medium">{{ t(`${prefix}.enabled`) }}</label>
          <Toggle id="capacity-enabled" v-model="form.enabled" />
        </div>
        <template v-if="form.enabled">
          <div class="flex items-center justify-between gap-4">
            <div>
              <label for="capacity-switch" class="text-sm font-medium">{{ t(`${prefix}.switchAccount`) }}</label>
              <p class="mt-1 text-xs text-gray-500">{{ t(`${prefix}.switchHint`) }}</p>
            </div>
            <Toggle id="capacity-switch" v-model="form.switch_account_on_overload" />
          </div>
          <div class="grid gap-4 sm:grid-cols-2">
            <div v-for="field in fields" :key="field.key">
              <label :for="`capacity-${field.key}`" class="mb-2 block text-sm font-medium">{{ t(`${prefix}.${field.key}`) }}</label>
              <input
                :id="`capacity-${field.key}`" v-model.number="form[field.key]" type="number" class="input"
                :min="field.key === 'max_cooldown_seconds' ? form.cooldown_seconds : field.min"
                :max="field.max" step="1" required
                @keydown.enter.stop.prevent="save"
              />
            </div>
          </div>
          <p class="text-xs text-gray-500 dark:text-gray-400">{{ t(`${prefix}.recoveryHint`) }}</p>
        </template>
        <button type="button" data-testid="capacity-save" class="btn btn-primary" :disabled="loading || loadFailed || saving" @click="save">
          {{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </fieldset>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { useAppStore } from '@/stores/app'
import { getOpenAICapacityCooldownSettings, updateOpenAICapacityCooldownSettings } from '@/api/admin/openaiCapacityCooldown'

const prefix = 'admin.settings.capacityCooldown'
const { t } = useI18n()
const app = useAppStore()
const card = ref<HTMLElement | null>(null)
const loading = ref(true)
const loadFailed = ref(false)
const saving = ref(false)
const form = reactive({
  enabled: false, switch_account_on_overload: true, window_seconds: 60,
  failure_threshold: 1, cooldown_seconds: 60, max_cooldown_seconds: 600
})
const fields = [
  { key: 'window_seconds', min: 1, max: 300 },
  { key: 'failure_threshold', min: 1, max: 100 },
  { key: 'cooldown_seconds', min: 1, max: 7200 },
  { key: 'max_cooldown_seconds', min: 1, max: 7200 }
] as const

async function load() {
  loading.value = true
  loadFailed.value = false
  try { Object.assign(form, await getOpenAICapacityCooldownSettings()) }
  catch { loadFailed.value = true }
  finally { loading.value = false }
}

async function save() {
  if (loading.value || loadFailed.value || saving.value) return
  const invalid = Array.from(card.value?.querySelectorAll('input') ?? []).find(input => !input.checkValidity())
  if (invalid) { invalid.reportValidity(); return }
  saving.value = true
  try {
    Object.assign(form, await updateOpenAICapacityCooldownSettings({ ...form }))
    app.showSuccess(t(`${prefix}.saved`))
  } catch { app.showError(t(`${prefix}.saveFailed`)) }
  finally { saving.value = false }
}

onMounted(load)
</script>
