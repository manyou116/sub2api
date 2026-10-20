import type { Account } from '@/types'

export interface WebImageSettings {
  model_mode: 'auto' | 'fixed'
  model: string
  thinking_effort: string
  require_download_attachment: boolean
}

export const isWebImageShadow = (account: Pick<Account, 'parent_account_id' | 'quota_dimension'> | null | undefined) =>
  account?.parent_account_id != null && account.quota_dimension === 'web_images'

export function readWebImageSettings(account: Account | null): WebImageSettings {
  const value = account?.extra?.openai_web_images as Partial<WebImageSettings> | undefined
  return {
    model_mode: value?.model_mode === 'fixed' ? 'fixed' : 'auto',
    model: typeof value?.model === 'string' ? value.model : 'gpt-5-6-thinking',
    thinking_effort: typeof value?.thinking_effort === 'string' ? value.thinking_effort : 'extended',
    require_download_attachment: value?.require_download_attachment === true
  }
}
