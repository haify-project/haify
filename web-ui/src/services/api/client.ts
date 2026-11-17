// Same-origin. The controller's UI server proxies /v1 to the REST gateway, so
// the browser never needs to know which port that is.
//
// This used to build an absolute URL to :3375 from window.location.hostname.
// That works only when the browser can reach the node directly. Put the UI
// behind anything that terminates TLS on 443 — a tunnel, a VPS, a reverse proxy
// — and every call goes to https://<public-name>:3375, a port that is not
// published and should not be: the page loads and nothing on it works.
const API_BASE = '/v1';

export interface ApiResponse<T = unknown> {
  success: boolean;
  message: string;
}

// API token storage for controllers with [auth] enabled. The token is kept
// in localStorage; on a 401/403 the registered prompt handler (a proper
// dialog mounted by the app shell) asks for a token once and the request is
// retried — no separate login page is needed for this single-admin model.
const TOKEN_STORAGE_KEY = 'haify_api_token';

export function getApiToken(): string {
  return localStorage.getItem(TOKEN_STORAGE_KEY) ?? '';
}

export function setApiToken(token: string): void {
  if (token) {
    localStorage.setItem(TOKEN_STORAGE_KEY, token);
  } else {
    localStorage.removeItem(TOKEN_STORAGE_KEY);
  }
}

// authPromptHandler resolves to true when the user supplied a (new) token
// and the failed request should be retried. The app shell registers a
// dialog-based handler; window.prompt is the headless fallback.
type AuthPromptHandler = () => Promise<boolean>;
let authPromptHandler: AuthPromptHandler | null = null;

export function setAuthPromptHandler(handler: AuthPromptHandler | null): void {
  authPromptHandler = handler;
}

async function promptForToken(): Promise<boolean> {
  if (authPromptHandler) {
    return authPromptHandler();
  }
  const entered = window.prompt(
    'Haify API token required (controller has authentication enabled):',
    getApiToken()
  );
  if (entered === null) return false;
  setApiToken(entered.trim());
  return true;
}

export type RequestFn = <T>(endpoint: string, options?: RequestInit) => Promise<T>;

function authHeaders(): Record<string, string> {
  const token = getApiToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

export function createRequest(baseUrl: string = API_BASE): RequestFn {
  return async function request<T>(
    endpoint: string,
    options?: RequestInit
  ): Promise<T> {
    const url = `${baseUrl}${endpoint}`;
    const doFetch = () =>
      fetch(url, {
        headers: {
          'Content-Type': 'application/json',
          ...authHeaders(),
          ...options?.headers,
        },
        mode: 'cors',
        ...options,
      });

    let response = await doFetch();

    if (response.status === 401 || response.status === 403) {
      if (await promptForToken()) {
        response = await doFetch();
      }
    }

    if (!response.ok) {
      const errorText = await response.text();
      throw new Error(`API error: ${response.status} ${errorText || response.statusText}`);
    }

    return response.json() as Promise<T>;
  };
}

/**
 * What a reply says beyond its plain confirmation, or undefined when it says
 * nothing more. The controller's reply to a resize or a snapshot can carry
 * what the person has to know: that a gateway's filesystem was grown, that a
 * running guest still sees the old size, that a snapshot is crash-consistent.
 */
export function replyNote(message: string | undefined, plain: string): string | undefined {
  const text = (message ?? '').trim();
  return text && text !== plain ? text : undefined;
}

// queryString renders only the parameters that were actually set. Sending
// empty values would be read by the server as filters rather than as absence.
export function queryString(params: object): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '' || v === false) continue;
    q.set(k, String(v));
  }
  const s = q.toString();
  return s ? `?${s}` : '';
}
