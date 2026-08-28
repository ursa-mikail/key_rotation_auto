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

export type RotationOutcome = 'rotated' | 'revoked_rotated' | 'revoked_terminated' | 'skipped' | 'failed' | 'in_progress';
export type RotationTrigger = 'timer' | 'revoke';

export interface KeysetSummary {
  keysetId: string;
  index: number;
  terminated: boolean;
  generation: number;
  primaryKeyId: string;
  // "rotating" | "terminated"
  status: 'rotating' | 'terminated';
  lastEventOutcome?: RotationOutcome;
  lastEventTrigger?: RotationTrigger;
  lastEventAt?: string;
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
  trigger: RotationTrigger;
  triggeredAt: string;
  fromKeyId: string | null;
  toKeyId: string | null;
  outcome: RotationOutcome;
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

  keysets: KeysetSummary[];
  keys: KeyRecord[];

  // Kill switch. Global -- stops the timer loop AND the revoke
  // interceptor loop at once.
  paused: boolean;
  pausedAt?: string;
  pausedReason?: string;

  // true = a revoke immediately emergency-rotates (and the keyset
  //        resumes normal cycling); false = a revoke permanently
  //        halts that keyset. Live-toggleable via /api/revoke-mode.
  revokeAutoRotate: boolean;

  tfVars: ArtifactView;
  terraformOutput: ArtifactView;
  tfVarsHash?: string;
  terraformInSync?: boolean;
  tfConfig: ArtifactView;
  renderedResource: ArtifactView;

  history: RotationEventRecord[];
  genesisAttempts: GenesisAttemptRecord[];
}

export interface GenesisBatchResult {
  status: 'created' | 'already-initialized';
  keysetCount: number;
}

export interface RevokeResult {
  keysetId: string;
  mode: 'auto-rotate' | 'halt';
  rotated: boolean;
  terminated: boolean;
  skipped?: string;
  fromKeyId?: string;
  toKeyId?: string;
}
