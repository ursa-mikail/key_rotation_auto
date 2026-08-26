export interface KeyRecord {
  keyId: string;
  generation: number;
  parentKeyId: string | null;
  createdAt: string;
  status: 'pending' | 'verified' | 'primary' | 'retired';
  verifiedAt?: string;
}

export interface ArtifactView {
  path: string;
  exists: boolean;
  content?: string;
  lastModified?: string;
}

export interface RotationEventRecord {
  rotationId: string;
  triggeredAt: string;
  fromKeyId: string | null;
  toKeyId: string | null;
  applied: boolean;
  reason: string;
}

export interface GenesisAttemptRecord {
  id: number;
  receivedAt: string;
  attemptedKeyId?: string;
  clientCreatedAt?: string;
  remoteAddr?: string;
  outcome: 'created' | 'already-initialized' | 'rejected' | 'invalid';
  detail?: string;
}

export interface StatusResponse {
  primaryKeyId: string | null;
  generation: number;
  lastRotatedAt: string | null;
  intervalSeconds: number;
  nextRotationInSeconds: number;
  keys: KeyRecord[];
  lastRotationId?: string;
  lastTestPassed?: boolean;
  lastTestDetail?: string;

  // Kill switch state, authoritative from rotation_state in Postgres.
  paused: boolean;
  pausedAt?: string;
  pausedReason?: string;

  // The actual files each loop reads/writes, read fresh off the
  // shared volume on every status tick so the UI always shows what's
  // really on disk, not a cached copy.
  tfVars: ArtifactView;
  terraformOutput: ArtifactView;
  tfVarsHash?: string;
  lastAppliedHash?: string;
  terraformInSync?: boolean;

  // The static terraform/main.tf source, for reference next to the
  // json snapshots above.
  tfConfig: ArtifactView;

  // Append-only records, most recent first.
  history: RotationEventRecord[];
  genesisAttempts: GenesisAttemptRecord[];
}

// The birth certificate the frontend generates for the genesis key,
// before the backend has ever seen it.
export interface KeyBirthCert {
  keyId: string;
  createdAt: string;
  algorithm: string;
  generation: number;
  parentKeyId: string | null;
  material: string; // base64, client-generated
}
