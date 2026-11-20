import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import en from '@/i18n/locales/en/dashboard'
import zh from '@/i18n/locales/zh/dashboard'
import UpstreamRequestBodyCell from '../UpstreamRequestBodyCell.vue'

describe('UpstreamRequestBodyCell', () => {
  for (const locale of ['en', 'zh']) {
    it(`shows weighted savings and fallback details in ${locale}`, () => {
      const wrapper = mount(UpstreamRequestBodyCell, {
        props: { stats: { raw_bytes: 2048, encoded_bytes: 1280, attempts: 2, compressed_attempts: 1, fallbacks: 1, encoding: 'identity', reason: 'encoding_rejected', final_raw_bytes: 1024, final_encoded_bytes: 1024 } },
        global: { plugins: [createI18n({ legacy: false, locale, messages: { en, zh } })] },
      })
      expect(wrapper.text()).toContain('2.0 KiB → 1.3 KiB')
      expect(wrapper.text()).toContain('37.5%')
      expect(wrapper.text()).toContain('identity')
      expect(wrapper.text()).not.toContain('usage.requestBody.')
      expect(wrapper.find('details').exists()).toBe(true)
    })
  }
  it('does not fabricate zero traffic for historical or unmeasured rows', () => {
    const wrapper = mount(UpstreamRequestBodyCell, {
      global: { plugins: [createI18n({ legacy: false, locale: 'en', messages: { en } })] },
    })
    expect(wrapper.text()).toBe('—')
    expect(wrapper.find('[title]').attributes('title')).toContain('No data')
  })
})
