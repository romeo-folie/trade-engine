/**
 * APP_VERSION — single source of truth for the user-facing app version on the
 * frontend. Seeds UI surfaces (boot screen, top bar, config footer) so they show
 * a real version immediately rather than a placeholder while /api/system/status
 * is still in flight. The backend (/api/system/status) reports the authoritative
 * value at runtime and overrides this where surfaces fetch it.
 *
 * Keep in sync with the version reported by the backend handler.
 */
export const APP_VERSION = '2.1.0';
