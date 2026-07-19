/** ChatGPT Web image upstream model + thinking options (aligned with chatgpt.com picker). */

export type OpenAIWebImagesModelMode = 'auto' | 'fixed'

export interface OpenAIWebImagesOption {
  value: string
  label: string
  /** Hint for plan availability */
  planHint?: 'all' | 'plus' | 'pro'
}

/** Recommended default used by auto mode / verified path. */
export const OPENAI_WEB_IMAGES_DEFAULT_MODEL = 'gpt-5-6-thinking'
export const OPENAI_WEB_IMAGES_DEFAULT_EFFORT = 'extended'

/**
 * Upstream model slugs accepted by /backend-api/f/conversation.
 * Labels mirror the official model picker naming.
 */
export const OPENAI_WEB_IMAGES_MODEL_OPTIONS: OpenAIWebImagesOption[] = [
  { value: OPENAI_WEB_IMAGES_DEFAULT_MODEL, label: 'GPT-5.6 Thinking (推荐/已验证)', planHint: 'plus' },
  { value: 'gpt-5-4', label: 'GPT-5.4 Instant', planHint: 'all' },
  { value: 'gpt-5-4-thinking', label: 'GPT-5.4 Thinking', planHint: 'plus' },
  { value: 'gpt-5-4-pro', label: 'GPT-5.4 Pro', planHint: 'pro' },
  { value: 'gpt-5-3', label: 'GPT-5.3 Instant', planHint: 'all' },
  { value: 'gpt-5-2', label: 'GPT-5.2 Instant', planHint: 'all' },
  { value: 'gpt-5-2-thinking', label: 'GPT-5.2 Thinking', planHint: 'plus' },
  { value: 'gpt-5-2-pro', label: 'GPT-5.2 Pro', planHint: 'pro' },
  { value: 'auto', label: 'Auto（官网自动）', planHint: 'all' }
]

/**
 * thinking_effort values used by ChatGPT web payload.
 * Pro models expose "pro" depth in the official UI.
 */
export const OPENAI_WEB_IMAGES_EFFORT_OPTIONS: OpenAIWebImagesOption[] = [
  { value: 'minimal', label: 'Minimal', planHint: 'all' },
  { value: 'low', label: 'Low', planHint: 'all' },
  { value: 'standard', label: 'Standard', planHint: 'all' },
  { value: 'medium', label: 'Medium', planHint: 'all' },
  { value: 'high', label: 'High', planHint: 'plus' },
  { value: 'extended', label: 'Extended（推荐）', planHint: 'plus' },
  { value: 'max', label: 'Max', planHint: 'pro' },
  { value: 'pro', label: 'Pro', planHint: 'pro' },
  { value: 'xhigh', label: 'xHigh', planHint: 'pro' }
]
