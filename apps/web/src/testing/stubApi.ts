// A stubbed Connect client set: every RPC is a `vi.fn` that rejects until a test says what it answers, so a call a test
// did not expect fails loudly. Test support: nothing in the app imports this file.
import { create } from '@bufbuild/protobuf';
import {
  GetMeResponseSchema,
  ListChannelsResponseSchema,
  ListMembersResponseSchema,
  ListMessagesResponseSchema,
  ListMeetingsResponseSchema,
  ListTasksResponseSchema,
  ListUpcomingResponseSchema,
  ListUsersResponseSchema,
  type CalendarEvent,
  type Channel,
  type GetMeResponse,
  type Meeting,
  type Member,
  type Message,
  type Task,
  type User,
} from '@kiwi/api-client';
import { vi, type Mock } from 'vitest';
import type { Api } from '../api/client';
import { MEMBERS, USERS, me } from './fixtures';

type Stub<Service> = {
  [Method in keyof Service]: Service[Method] extends (...args: infer Args) => infer Result ? Mock<(...args: Args) => Result> : never;
};

export type StubApi = { [Name in keyof Api]: Stub<Api[Name]> };

/** One service whose methods come into being, as rejecting mocks, the first time a test or the app asks for them. */
function stubService<Service extends object>(name: string): Stub<Service> {
  const methods = new Map<string | symbol, Mock>();
  return new Proxy({} as Stub<Service>, {
    get(_target, method) {
      let stub = methods.get(method);
      if (stub === undefined) {
        stub = vi.fn(() => Promise.reject(new Error(`Unexpected call to ${name}.${String(method)}`)));
        methods.set(method, stub);
      }
      return stub;
    },
  });
}

export function createStubApi(): StubApi {
  return {
    agents: stubService('agents'),
    chat: stubService('chat'),
    identity: stubService('identity'),
    tasks: stubService('tasks'),
    files: stubService('files'),
    meetings: stubService('meetings'),
    calendar: stubService('calendar'),
  };
}

export type World = {
  channels?: Channel[];
  members?: Member[];
  users?: User[];
  /** The latest page, in the order the server sends it: newest first. */
  messages?: Message[];
  tasks?: Task[];
  /** The upcoming calendar events `ListUpcoming` answers. */
  events?: CalendarEvent[];
  /** The meetings `ListMeetings` answers. */
  meetings?: Meeting[];
  /** The signed-in user's answer to `GetMe`, when a test needs another (the email, say). */
  me?: GetMeResponse;
  /** `GetMe` never answers: the signed-in user is not loaded yet. */
  meNeverLoads?: boolean;
};

/** Answers the reads every channel screen starts with. A test overrides single calls afterwards. */
export function seedApi(api: StubApi, world: World = {}): StubApi {
  api.identity.getMe.mockImplementation(() => (world.meNeverLoads === true ? new Promise<never>(() => undefined) : Promise.resolve(world.me ?? create(GetMeResponseSchema, me()))));
  api.identity.listUsers.mockResolvedValue(create(ListUsersResponseSchema, { users: world.users ?? USERS }));
  api.chat.listChannels.mockResolvedValue(create(ListChannelsResponseSchema, { channels: world.channels ?? [] }));
  api.chat.listMembers.mockResolvedValue(create(ListMembersResponseSchema, { members: world.members ?? MEMBERS }));
  api.chat.listMessages.mockResolvedValue(create(ListMessagesResponseSchema, { messages: world.messages ?? [] }));
  api.tasks.listTasks.mockResolvedValue(create(ListTasksResponseSchema, { tasks: world.tasks ?? [] }));
  api.calendar.listUpcoming.mockResolvedValue(create(ListUpcomingResponseSchema, { events: world.events ?? [] }));
  api.meetings.listMeetings.mockResolvedValue(create(ListMeetingsResponseSchema, { meetings: world.meetings ?? [] }));
  return api;
}
