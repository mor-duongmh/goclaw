export const REASONING_DELIVERY_VALUES = ["streaming_only", "always_bubbles", "off"] as const;

export type ReasoningDeliveryMode = (typeof REASONING_DELIVERY_VALUES)[number];

export const reasoningDeliveryOptions: { value: ReasoningDeliveryMode; label: string }[] = [
  { value: "streaming_only", label: "Streaming only" },
  { value: "always_bubbles", label: "Always as bubbles" },
  { value: "off", label: "Off" },
];

// Telegram shipped reasoning streaming before the three-mode config existed, so
// an unrecognizable value there keeps its historical meaning. Slack reasoning is
// opt-in from the start, so anything unrecognizable must stay off — mirrors
// unrecognizedReasoningDeliveryMode in internal/config/channel_reasoning_delivery.go.
const CHANNEL_FALLBACK: Record<string, ReasoningDeliveryMode> = { slack: "off" };
const DEFAULT_FALLBACK: ReasoningDeliveryMode = "streaming_only";

export function reasoningDeliveryFallback(channelType?: string): ReasoningDeliveryMode {
  return (channelType && CHANNEL_FALLBACK[channelType]) || DEFAULT_FALLBACK;
}

export function resolveReasoningDeliveryValue(
  config: Record<string, unknown> | undefined,
  channelType?: string,
): ReasoningDeliveryMode {
  const raw = typeof config?.reasoning_delivery === "string" ? config.reasoning_delivery : "";
  const mode = canonicalReasoningDeliveryMode(raw);
  if (mode) return mode;
  if (config?.reasoning_stream === false) return "off";
  return reasoningDeliveryFallback(channelType);
}

export function normalizeReasoningDeliveryConfig<T extends Record<string, unknown>>(
  config: T,
  channelType?: string,
): T {
  const next = { ...config } as Record<string, unknown>;
  if (next.reasoning_delivery !== undefined || next.reasoning_stream !== undefined) {
    next.reasoning_delivery = resolveReasoningDeliveryValue(next, channelType);
    delete next.reasoning_stream;
  }
  return next as T;
}

// Stray case or whitespace is a typo, not a different intent. The backend
// canonicalizes the same way, so discarding those here would make the saved
// config and the rendered form disagree.
function canonicalReasoningDeliveryMode(value: string): ReasoningDeliveryMode | null {
  const normalized = value.trim().toLowerCase();
  return (REASONING_DELIVERY_VALUES as readonly string[]).includes(normalized)
    ? (normalized as ReasoningDeliveryMode)
    : null;
}
