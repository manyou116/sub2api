import { apiClient } from '@/api/client'

export type MonitorRange = '24h' | '7d' | '15d'
export type DemandBasisKey = 'last24h' | 'last7d' | 'previous24h'

export interface CapacityHistoryPoint {
  start_at: string
  end_at: string
  complete: boolean
  pool_account_cost: number | null
  group_route_cost: number | null
  pool_via_group_cost: number | null
  pool_other_cost: number | null
  pool_requests: number
  group_route_requests: number
}

export interface CapacityPoint {
  at: string
  hours: number
  capacity: number | null
  direct_capacity: number | null
  estimated_capacity: number | null
  account_count: number
}

export interface CapacityMilestone {
  hours: number
  capacity: number | null
  demand: number | null
  gap: number | null
}

export interface CapacityDemandBasis {
  key: DemandBasisKey
  hours: number
  cost: number | null
  rate_per_hour: number | null
  available: boolean
  reason?: string
}

export interface CapacityScenario {
  key: DemandBasisKey
  first_shortfall_at: string | null
  minimum_gap: number | null
  milestones: CapacityMilestone[]
}

export interface GroupCapacityMonitor {
  as_of: string
  timezone: string
  currency: string
  fee_basis: string
  model_version: string
  scope?: string
  membership_as_of?: string
  inventory: {
    member_count: number
    eligible_count: number
    schedulable_count: number
    modeled_count: number
    direct_count: number
    estimated_count: number
    stale_count: number
    missing_window_count: number
    shared_count: number
    history_account_count: number
    unknown_identity_count: number
    provenance_unknown_count: number
    state_excluded_count: number
    duplicate_count: number
    [key: string]: number
  }
  quality: {
    history_completeness: string
    window_provenance: string
    identity_quality: string
    freshest_sample_at?: string | null
    oldest_sample_at?: string | null
    unclassified_cost: number
  }
  history: CapacityHistoryPoint[]
  demand_bases: CapacityDemandBasis[]
  capacity_points: CapacityPoint[]
  scenarios: CapacityScenario[]
  shared_usage: { group_id: number | null; group_name?: string; cost: number }[]
  limitations: string[]
  cache: { expires_at: string }
}

export async function getGroupCapacityMonitor(id: number, signal?: AbortSignal): Promise<GroupCapacityMonitor> {
  const { data } = await apiClient.get<GroupCapacityMonitor>(`/admin/groups/${id}/capacity-monitor`, {
    params: { history: '15d', horizon: '15d' }, signal,
  })
  return data
}
