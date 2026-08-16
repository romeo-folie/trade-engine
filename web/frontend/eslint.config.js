import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import tseslint from 'typescript-eslint'
import { defineConfig } from 'eslint/config'

export default defineConfig(
  {
    files: ['src/**/*.{ts,tsx}'],
    // Initial adoption intentionally starts below the full type-checked and
    // React Compiler presets. Those currently expose broad legacy API-boundary
    // and state-management debt; this gate enforces the core recommended rules
    // plus high-signal async and Rules of Hooks checks without blanket inline
    // suppressions. Expand the preset as that debt is typed and refactored.
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
    ],
    plugins: {
      'react-hooks': reactHooks,
    },
    languageOptions: {
      globals: globals.browser,
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    rules: {
      '@typescript-eslint/await-thenable': 'error',
      '@typescript-eslint/no-floating-promises': 'error',
      '@typescript-eslint/no-misused-promises': 'error',
      'react-hooks/exhaustive-deps': 'error',
      'react-hooks/rules-of-hooks': 'error',
    },
  },
  {
    files: ['tests/**/*.ts', 'vite.config.ts'],
    extends: [js.configs.recommended, tseslint.configs.recommended],
    languageOptions: {
      globals: globals.node,
    },
  },
)
