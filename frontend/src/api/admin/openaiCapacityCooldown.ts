import apiClient from '../client'

export interface OpenAICapacityCooldownSettings {
  enabled: boolean
  switch_account_on_overload: boolean
  window_seconds: number
  failure_threshold: number
  cooldown_seconds: number
  max_cooldown_seconds: number
  slow_first_output_enabled: boolean
  slow_first_output_seconds: number
  slow_first_output_high_effort_seconds: number
  slow_first_output_window_seconds: number
  slow_first_output_threshold: number
}

const path = '/admin/settings/openai-capacity-cooldown'

export async function getOpenAICapacityCooldownSettings(): Promise<OpenAICapacityCooldownSettings> {
  return (await apiClient.get<OpenAICapacityCooldownSettings>(path)).data
}

export async function updateOpenAICapacityCooldownSettings(value: OpenAICapacityCooldownSettings): Promise<OpenAICapacityCooldownSettings> {
  return (await apiClient.put<OpenAICapacityCooldownSettings>(path, value)).data
}
