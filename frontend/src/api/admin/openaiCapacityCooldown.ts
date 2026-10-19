import apiClient from '../client'

export interface OpenAICapacityCooldownSettings {
  enabled: boolean
  switch_account_on_overload: boolean
  window_seconds: number
  failure_threshold: number
  failure_rate_percent: number
  cooldown_seconds: number
  max_cooldown_seconds: number
}

const path = '/admin/settings/openai-capacity-cooldown'

export async function getOpenAICapacityCooldownSettings(): Promise<OpenAICapacityCooldownSettings> {
  return (await apiClient.get<OpenAICapacityCooldownSettings>(path)).data
}

export async function updateOpenAICapacityCooldownSettings(value: OpenAICapacityCooldownSettings): Promise<OpenAICapacityCooldownSettings> {
  return (await apiClient.put<OpenAICapacityCooldownSettings>(path, value)).data
}
