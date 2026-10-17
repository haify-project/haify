import { type ApiResponse, type RequestFn } from './client';

/** One finding of a cluster inspection (`haify inspect`). */
export interface InspectionCheck {
  id: string;
  /** resources | gateways | nodes | pools | backups | alerts | selfha | tls | hygiene */
  area: string;
  /** The resource, node or channel concerned; empty for an area-wide line. */
  subject?: string;
  /** pass | warn | fail | error */
  status: string;
  message: string;
  evidence?: string[];
  fix?: string;
  runbook?: string;
}

export interface InspectionReport {
  id: string;
  trigger: string;
  /** Epoch milliseconds, a 64-bit integer, so it arrives as a string. */
  startedAtUnixMs?: string;
  finishedAtUnixMs?: string;
  summary?: { pass?: number; warn?: number; fail?: number; error?: number };
  checks?: InspectionCheck[];
}

export interface InspectionResponse extends ApiResponse {
  report?: InspectionReport;
}

export const inspectApi = (request: RequestFn) => ({
  getLatestInspection: () => request<InspectionResponse>('/inspections/latest'),
});
