import type { Account } from '@/types'
import type { ImageShadowMode } from '@/api/admin/accounts'

export interface WebImageSettings {
  model_mode: 'recommended' | 'fixed'
  model: string
  thinking_effort: string
  require_download_attachment: boolean
}

type ShadowIdentity = Pick<Account, 'parent_account_id' | 'quota_dimension'>
export const isWebImageShadow = (account: ShadowIdentity | null | undefined) =>
  account?.parent_account_id != null && account.quota_dimension === 'web_images'
export const isWorkImageShadow = (account: ShadowIdentity | null | undefined) =>
  account?.parent_account_id != null && account.quota_dimension === 'work_images'
export const isImageShadow = (account: ShadowIdentity | null | undefined) =>
  isWebImageShadow(account) || isWorkImageShadow(account)
export const imageShadowMode = (account: ShadowIdentity | null | undefined): ImageShadowMode =>
  isWorkImageShadow(account) ? 'work' : 'web'

export function readWebImageSettings(account: Account | null): WebImageSettings {
  const value = account?.extra?.openai_web_images as Partial<WebImageSettings> | undefined
  return {
    model_mode: value?.model_mode === 'fixed' ? 'fixed' : 'recommended',
    model: typeof value?.model === 'string' ? value.model : '',
    thinking_effort: typeof value?.thinking_effort === 'string' ? value.thinking_effort : '',
    require_download_attachment: isWorkImageShadow(account) || value?.require_download_attachment === true
  }
}
