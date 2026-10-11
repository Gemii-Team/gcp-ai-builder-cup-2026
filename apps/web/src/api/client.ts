import { createClient, type Client } from '@connectrpc/connect';
import {
  AgentService,
  ChatService,
  createApiTransport,
  FileService,
  IdentityService,
  MeetingService,
  TaskService,
} from '@kiwi/api-client';
import { createContext, createElement, useContext, type ReactNode } from 'react';

/** The API is same-origin: the dev server proxies `/api` and `/ws`, and in the cluster the ingress does. */
export const API_BASE_URL = '/api';

export type Api = {
  agents: Client<typeof AgentService>;
  chat: Client<typeof ChatService>;
  identity: Client<typeof IdentityService>;
  tasks: Client<typeof TaskService>;
  files: Client<typeof FileService>;
  meetings: Client<typeof MeetingService>;
};

export function createApi(baseUrl: string = API_BASE_URL): Api {
  const transport = createApiTransport(baseUrl);
  return {
    agents: createClient(AgentService, transport),
    chat: createClient(ChatService, transport),
    identity: createClient(IdentityService, transport),
    tasks: createClient(TaskService, transport),
    files: createClient(FileService, transport),
    meetings: createClient(MeetingService, transport),
  };
}

const ApiContext = createContext<Api | null>(null);

/** Hands the service clients to the screens below it; tests pass stubs here. */
export function ApiProvider({ api, children }: { api: Api; children: ReactNode }) {
  return createElement(ApiContext, { value: api }, children);
}

export function useApi(): Api {
  const api = useContext(ApiContext);
  if (api === null) {
    throw new Error('useApi needs an <ApiProvider> above it.');
  }
  return api;
}
