export type IpType = "residential" | "mobile" | "hosting" | "unknown";
export type NodeStatus = "discovered" | "probing" | "ready" | "unavailable" | "cooldown";
export type PolicyMode = "auto" | "speed_first" | "smart" | "residential_first" | "country" | "fixed" | "favorites";
export type RoutingIpType = "all" | "residential" | "hosting";

export interface AccessConfig {
  web_external_access: boolean;
  proxy_external_access: boolean;
  proxy_auth_configured: boolean;
}

// Identity and reachability — the whole of what the console configures. Tuning
// values (intervals, timeouts, concurrency, data sources) are constants in the
// Go binary and never travel over this API.
export interface AppSettings {
  admin: {
    username: string; secret_path: string; web_port: number;
    web_external_access: boolean; password_set: boolean;
  };
  proxy: {
    enabled: boolean; port: number; username: string; external_access: boolean;
    password_set: boolean;
  };
}

export interface ProxyNode {
  id: string;
  provider: string;
  country: string;
  country_code: string;
  country_zh: string;
  country_flag: string;
  host_name: string;
  ip_address: string;
  remote_host: string;
  remote_port: number;
  transport: string;
  ip_type: IpType;
  owner: string;
  as_name: string;
  location: string;
  status: NodeStatus;
  source_score: number;
  source_ping_ms: number;
  source_speed_bps: number;
  source_sessions: number;
  latency_ms: number;
  success_count: number;
  failure_count: number;
  source_present: boolean;
  last_probed_at: string | null;
  last_success_at: string | null;
}

export interface CountryFacet {
  code: string;
  country: string;
  country_zh: string;
  country_flag: string;
  total: number;
  ready: number;
}

export interface ProxyNodePage {
  items: ProxyNode[];
  total: number;
  limit: number;
  offset: number;
}

export interface GatewayStatus {
  running: boolean;
  active_node_id: string | null;
  tunnel_status: string;
  proxy_listener: string;
  last_error: string | null;
  active_latency_ms: number;
  exit_ip: string | null;
  exit_latency_ms: number;
  connection_enabled: boolean;
}

export interface PoolStatistics {
  total: number;
  ready: number;
  discovered: number;
  unavailable: number;
  cooldown: number;
  residential: number;
  mobile: number;
  hosting: number;
  unknown: number;
  favorites: number;
  blacklisted: number;
  countries: number;
}

export interface ProxySettings {
  routing_mode: PolicyMode;
  force_country: string;
  routing_ip_type: RoutingIpType;
  connection_enabled: boolean;
  fixed_node_id: string | null;
  favorite_node_ids: string[];
}

export interface Job {
  id: string;
  name: string;
  status: string;
  error: string | null;
  result: Record<string, unknown> | null;
}

export interface DiagnosticCheck {
  name: string;
  ok: boolean;
  detail: string;
  severity: string;
  recoverable: boolean;
}

export interface SystemDiagnostics {
  healthy: boolean;
  checks: DiagnosticCheck[];
}

export interface SystemStatus {
  name: string;
  version: string;
  environment: string;
  status: string;
  nodes: number;
  gateway_running: boolean;
  active_node_id: string | null;
  listeners: Record<string, string>;
  monitors: Record<string, boolean>;
  monitor_details: Record<string, Record<string, unknown>>;
}

// One group of changes in a release ("新功能", "问题修复", ...). The server
// builds these from the release notes, so the console only renders them.
export interface ReleaseSection {
  title: string;
  items: string[];
}

export interface ReleaseNotes {
  version: string;
  published_at: string;
  url: string;
  sections: ReleaseSection[] | null;
}

export interface UpdateStatus {
  current_version: string;
  latest_version: string;
  update_available: boolean;
  // Every release between the installed version and the newest one, newest
  // first — an update installs the newest and brings all of them.
  pending: ReleaseNotes[] | null;
  supported: boolean;
  unsupported_reason: string;
  updating: boolean;
  checked_at: string;
  releases_url: string;
  last_log: string;
}

export interface LogEntry {
  timestamp: string;
  level: string;
  module: string;
  message: string;
}

export interface AuthConfig {
  username: string;
  secret_path: string;
  host: string;
  port: number;
  proxy_host: string;
  proxy_port: number;
  password_set: boolean;
}

export interface ProxyHealthResult {
  ok: boolean;
  exit_ip: string | null;
  latency_ms: number;
  error: string | null;
}
