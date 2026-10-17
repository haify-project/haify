// Build-time switches. __HAIFY_AI__ is replaced with a literal by rsbuild
// (rsbuild.config.ts), so code behind a false switch is left out of the bundle.
declare const __HAIFY_AI__: boolean;

/** The Copilot and its settings page, built in with `make build AI=1`. */
export const AI_ENABLED: boolean = __HAIFY_AI__;

declare const __HAIFY_VERSION__: string;

/** The Haify release this UI was built from, e.g. "v1.15.0" or "v1.15.0-3-gabc1234". */
export const HAIFY_VERSION: string = __HAIFY_VERSION__;
