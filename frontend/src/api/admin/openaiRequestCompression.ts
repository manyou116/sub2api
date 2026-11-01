import apiClient from '../client'

export interface OpenAIRequestCompressionSettings {
  enabled: boolean
  image_guard_enabled?: boolean
  image_guard_max_bytes?: number
  bare_400_fallback_enabled?: boolean
}
const path = '/admin/settings/openai-request-compression'

export async function getOpenAIRequestCompression(): Promise<OpenAIRequestCompressionSettings> {
  return (await apiClient.get<OpenAIRequestCompressionSettings>(path)).data
}

export async function updateOpenAIRequestCompression(settings: Partial<OpenAIRequestCompressionSettings>): Promise<OpenAIRequestCompressionSettings> {
  return (await apiClient.put<OpenAIRequestCompressionSettings>(path, settings)).data
}
