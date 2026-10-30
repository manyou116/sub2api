import apiClient from '../client'

export interface OpenAIRequestCompressionSettings { enabled: boolean }
const path = '/admin/settings/openai-request-compression'

export async function getOpenAIRequestCompression(): Promise<OpenAIRequestCompressionSettings> {
  return (await apiClient.get<OpenAIRequestCompressionSettings>(path)).data
}

export async function updateOpenAIRequestCompression(enabled: boolean): Promise<OpenAIRequestCompressionSettings> {
  return (await apiClient.put<OpenAIRequestCompressionSettings>(path, { enabled })).data
}
