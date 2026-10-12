// Entry point of @kiwi/api-client: the Connect-ES v2 service descriptors, the
// message types and schemas generated from proto/, the realtime frame types and
// the transport. Use `createClient(ChatService, createApiTransport(baseUrl))`.
// Domain events (kiwi.events.v1) are backend-internal and not exported.
export { createApiTransport } from './transport';

export * from './gen/kiwi/identity/v1/identity_pb';
export * from './gen/kiwi/chat/v1/artifact_pb';
export * from './gen/kiwi/chat/v1/chat_pb';
export * from './gen/kiwi/tasks/v1/tasks_pb';
export * from './gen/kiwi/files/v1/files_pb';
export * from './gen/kiwi/meetings/v1/meetings_pb';
export * from './gen/kiwi/agents/v1/agents_pb';
export * from './gen/kiwi/realtime/v1/realtime_pb';

// knowledge.v1.Citation shares its name with chat.v1.Citation, so the knowledge
// messages are namespaced instead of re-exported flat.
export * as knowledge from './gen/kiwi/knowledge/v1/knowledge_pb';
