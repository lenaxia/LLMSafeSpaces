package com.llmsafespaces.sdk.models;

/**
 * The platform-owned view of one agent session (pkg/session contract,
 * design 0049) — the getSession response shape.
 */
public class Session {
    public String id;
    public String workspaceId;
    public String parentId;
    public String title;
    public String agentId;
    public ModelRef model;
    /** unknown | idle | busy | error | compacting | archived */
    public String status;
    public Cost cost;
    public ContextUsage contextUsage;
    public TimeRange time;
    public String summary;
    public boolean archived;

    /** Identifies a model an adapter selected for a session or message. */
    public static class ModelRef {
        public String id;
        public String provider;
    }

    /** Display-only token/cost data (never billing). */
    public static class Cost {
        public long inputTokens;
        public long outputTokens;
        public long reasoningTokens;
        public long cacheReadTokens;
        public long cacheWriteTokens;
        public long totalTokens;
        public double costUsd;
    }

    /** The session's live context occupancy (non-monotonic: compaction resets it). */
    public static class ContextUsage {
        public long used;
        public long window;
    }

    /** Bounds a session or message. completedAt is null while busy. */
    public static class TimeRange {
        public String startedAt;
        public String completedAt;
    }
}
