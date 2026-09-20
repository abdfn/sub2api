import { baseCompile } from '@intlify/message-compiler'
import type { MessageCompiler } from 'vue-i18n'

// Vitest uses the runtime-only i18n build. Compile shipped message strings when
// a component test needs to assert the text and interpolated values users see.
export const testMessageCompiler: MessageCompiler = (message, context) => {
  if (typeof message !== 'string') throw new TypeError('Expected a string test message')
  const { code } = baseCompile(message, { mode: 'arrow', onError: context.onError })
  return new Function(`return ${code}`)()
}
