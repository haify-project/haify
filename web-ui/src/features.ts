// Build-time switches. __HAIFY_AI__ is replaced with a literal by rsbuild
// (rsbuild.config.ts), so code behind a false switch is left out of the bundle.
declare const __HAIFY_AI__: boolean;

/** The Copilot and its settings page, built in with `make build AI=1`. */
export const AI_ENABLED: boolean = __HAIFY_AI__;
