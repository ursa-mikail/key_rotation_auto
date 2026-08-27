export interface KeyRecord {
  keyId: string;
  keysetId: string;
  generation: number;
  parentKeyId: string | null;
  createdAt: string;
  expiresAt: string;
  status: 'pending' | 'verified' | 'primary' | 'retired';
  verifiedAt?: string;
}

export interface KeysetSummary {
  keysetId: string;
  index: number;
  rotating: boolean;
  revoked: boolean;
  generation: number;
  primaryKeyId: string;
  // "static" | "pending-renewal" | "renewed" | "rotating"
  status: 'static' | 'pending-renewal' | 'renewed' | 'rotating';
  expiresAt?: string;
  nextRotationInSeconds?: number;
  minSeconds?: number;
  maxSeconds?: number;
}

export interface ArtifactView {
  path: string;
  exists: boolean;
  content?: string;
  lastModified?: string;
}

export interface RotationEventRecord {
  rotationId: string;
  keysetId: string;
  triggeredAt: string;
  fromKeyId: string | null;
  toKeyId: string | null;
  applied: boolean;
  reason: string;
}

export interface GenesisAttemptRecord {
  id: number;
  receivedAt: string;
  remoteAddr?: string;
  outcome: 'created' | 'already-initialized' | 'rejected';
  detail?: string;
}

export interface StatusResponse {
  initialized: boolean;
  keysetCount: number; // N
  rotatingCount: number; // M
  revokedCount: number; // L
  staticCount: number; // N - M

  keysets: KeysetSummary[];
  keys: KeyRecord[];

  // Kill switch state, authoritative from system_state in Postgres.
  // Global -- stops every rotating keyset at once.
  paused: boolean;
  pausedAt?: string;
  pausedReason?: string;

  // The actual files each loop reads/writes, read fresh off the
  // shared volume on every status tick so the UI always shows what's
  // really on disk, not a cached copy.
  tfVars: ArtifactView;
  terraformOutput: ArtifactView;
  tfVarsHash?: string;
  terraformInSync?: boolean;

  // The static terraform/main.tf source, for reference next to the
  // json snapshots above.
  tfConfig: ArtifactView;
  renderedResource: ArtifactView;

  // Append-only records, most recent first, across every keyset.
  history: RotationEventRecord[];
  genesisAttempts: GenesisAttemptRecord[];
}

export interface GenesisBatchResult {
  status: 'created' | 'already-initialized';
  keysetCount: number;
  rotatingCount: number;
  revokedCount: number;
  staticCount: number;
}
