import js from '@eslint/js'
import globals from 'globals'
import tseslint from 'typescript-eslint'
import react from 'eslint-plugin-react'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'

const SEC22 =
  'SEC-2.2: never persist the CSRF token or secrets. Hold them in memory only.'
const SEC23 =
  'SEC-2.2: the console does not use cookies; writing one would defeat the CSRF token.'

export default tseslint.config(
  { ignores: ['dist', 'node_modules'] },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      react,
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    settings: { react: { version: '19.0' } },
    rules: {
      ...react.configs.flat.recommended.rules,
      ...react.configs.flat['jsx-runtime'].rules,
      ...reactHooks.configs.recommended.rules,
      'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],

      // SEC-2.3: keys, values and error strings are attacker-controlled bytes. They must
      // only ever be rendered as React children, which auto-escape. This rule forbids the
      // one API that would bypass that.
      'react/no-danger': 'error',

      // SEC-2.2: the CSRF token must live in a module-level variable only. Any persistent
      // store is readable by XSS and leaks across tabs and origins.
      //
      // `no-restricted-globals` alone is NOT sufficient: it matches only the BARE
      // identifier, so `localStorage` is caught while `window.localStorage` and
      // `globalThis.localStorage` pass cleanly. Probing the config confirmed exactly that
      // bypass, so the persistent stores are matched by syntax instead, covering the bare
      // identifier, member access on any object, and computed string keys.
      'no-restricted-globals': [
        'error',
        { name: 'localStorage', message: SEC22 },
        { name: 'sessionStorage', message: SEC22 },
      ],
      'no-restricted-syntax': [
        'error',
        {
          // window.localStorage, globalThis.sessionStorage, foo.localStorage, and the
          // computed equivalents.
          selector:
            'MemberExpression[property.name=/^(localStorage|sessionStorage)$/]',
          message: SEC22,
        },
        {
          selector:
            'MemberExpression[property.type="Literal"][property.value=/^(localStorage|sessionStorage|cookie)$/]',
          message: SEC22,
        },
        {
          // document.cookie in any spelling, including document['cookie'].
          selector: 'MemberExpression[object.name="document"][property.name="cookie"]',
          message: SEC23,
        },
      ],
      'no-restricted-properties': [
        'error',
        {
          object: 'document',
          property: 'cookie',
          message: SEC23,
        },
      ],
    },
  },
)
