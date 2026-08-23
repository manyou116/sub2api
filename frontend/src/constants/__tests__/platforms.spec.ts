import { describe, expect, it } from 'vitest'
import {
  COMPOSITE_ROUTE_PLATFORM_OPTIONS,
  CONCRETE_PLATFORM_OPTIONS,
  GROUP_PLATFORM_OPTIONS
} from '@/constants/platforms'

const concretePlatforms = [
  'anthropic',
  'openai',
  'gemini',
  'antigravity',
  'grok',
  'kimi',
  'zhipu',
  'deepseek',
  'kiro'
]

const compositeRoutePlatforms = concretePlatforms.filter((platform) => platform !== 'kiro')

describe('platform option catalogs', () => {
  it('exposes every concrete account platform', () => {
    expect(CONCRETE_PLATFORM_OPTIONS.map((option) => option.value)).toEqual(concretePlatforms)
  })

  it('adds composite for group-backed filters', () => {
    expect(GROUP_PLATFORM_OPTIONS.map((option) => option.value)).toEqual([
      ...concretePlatforms,
      'composite'
    ])
  })

  it('keeps composite route targets aligned to backend-supported platforms', () => {
    expect(COMPOSITE_ROUTE_PLATFORM_OPTIONS.map((option) => option.value)).toEqual(compositeRoutePlatforms)
  })
})
