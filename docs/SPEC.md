# Server Specification

## Overview

aspirant-server is a Go/Gin HTTP API gateway providing authentication, file management, game logic, and reverse-proxy access to downstream microservices. It serves as the single backend entrypoint for the aspirant-online platform.

## Route Groups

### Public (no auth)

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| POST | `/login` | LoginHandler | Authenticate user, return JWT |
| GET | `/login:username` | LoginUserHandler | Check if username exists |
| GET | `/health` | HealthCheckHandler | Service health check |
| POST | `/games/word_weaver` | GetLongestWordsHandler | Process Word Weaver board |
| GET | `/fetch-object/:etag` | FetchObjectHandler | Fetch object by ETag |
| GET | `/games/scores` | GetGameScoresHandler | Get game leaderboard |
| POST | `/bootstrap/admin` | BootstrapUserHandler | Create first admin (only when no users exist) |

### Authenticated (any logged-in user)

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| GET | `/data_models/users/:id` | GetUserHandler | Get user by ID |
| GET | `/data_models/users` | GetAllUsersHandler | List all users |
| POST | `/games/scores` | SaveGameScoreHandler | Submit a game score |

### Trusted (Trusted or Admin role)

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| GET | `/data_models/message` | GetAllMessagesHandler | List messages |
| POST | `/data_models/message` | PostMessageHandler | Post a message |
| GET | `/files/list` | ListFilesHandler | List user's files |
| GET | `/files/shared/list` | ListSharedFilesHandler | List shared files |
| POST | `/files/upload` | UploadFileHandler | Upload a file |
| POST | `/files/shared/upload` | UploadSharedFileHandler | Upload to shared |
| GET | `/files/download/:filename` | DownloadFileHandler | Download a file |
| GET | `/files/shared/download/:filename` | DownloadSharedFileHandler | Download from shared |
| DELETE | `/files/delete/:filename` | DeleteFileHandler | Delete a file |
| POST | `/files/folder` | CreateFolderHandler | Create a folder |
| POST | `/files/shared/folder` | CreateSharedFolderHandler | Create shared folder |
| GET | `/files/usage/me` | OwnStorageUsageHandler | Own storage usage |
| GET | `/translator/health` | GetTranslatorHealthHandler | Translator health |
| GET | `/translator/languages` | GetTranslatorLanguagesHandler | List languages |
| POST | `/translator/languages/install` | InstallTranslatorLanguageHandler | Install a language |
| POST | `/translator/translations` | TranslateHandler | Translate text |
| POST | `/upload` | UploadImageHandler | Upload image |
| GET | `/data_models/ludde_feeding_times` | GetAllFeedingTimesHandler | List feeding times |
| GET | `/data_models/ludde_feeding_times/:id` | GetFeedingTimeHandler | Get feeding time |
| POST | `/data_models/ludde_feeding_times` | AddFeedingTimeHandler | Add feeding time |
| DELETE | `/data_models/ludde_feeding_times/:id` | DeleteFeedingTimeHandler | Delete feeding time |

### Admin (Admin role only)

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| GET | `/s3-assets` | ListS3AssetsHandler | List assets |
| GET | `/data_models/roles` | GetAllRolesHandler | List all roles |
| POST | `/data_models/users` | CreateUserHandler | Create a user |
| PUT | `/data_models/users/:id` | UpdateUserHandler | Update a user |
| DELETE | `/data_models/users/:id` | DeleteUserHandler | Delete a user |
| GET | `/files/usage` | StorageUsageHandler | All users' storage usage |
| DELETE | `/files/shared/delete/:filename` | DeleteSharedFileHandler | Delete shared file |
| GET | `/transcriber/health` | GetTranscriberHealthHandler | Transcriber health |
| GET | `/commander/health` | GetCommanderHealthHandler | Commander health |
| GET | `/voice-messages` | ListVoiceMessagesHandler | List voice messages |
| GET | `/voice-messages/:id` | GetVoiceMessageHandler | Get voice message |
| POST | `/voice-messages` | UploadVoiceMessageHandler | Upload voice message |
| DELETE | `/voice-messages/:id` | DeleteVoiceMessageHandler | Delete voice message |
| GET | `/voice-messages/:id/audio` | GetVoiceAudioHandler | Get voice audio |
| GET | `/commander/tasks` | ListCommanderTasksHandler | List tasks |
| GET | `/commander/tasks/:id` | GetCommanderTaskHandler | Get task |
| PATCH | `/commander/tasks/:id` | UpdateCommanderTaskHandler | Update task |
| DELETE | `/commander/tasks/:id` | DeleteCommanderTaskHandler | Delete task |
| POST | `/commander/process` | TriggerCommanderProcessHandler | Process voice commands |
| GET | `/commander/vocabulary` | GetCommanderVocabularyHandler | Get command vocabulary |
| GET | `/commander/notes` | ListCommanderNotesHandler | List notes |
| GET | `/commander/notes/:id` | GetCommanderNoteHandler | Get note |
| PATCH | `/commander/notes/:id` | UpdateCommanderNoteHandler | Update note |
| DELETE | `/commander/notes/:id` | DeleteCommanderNoteHandler | Delete note |

## RBAC

Six roles seeded at startup:

| Role | ID | Description |
|------|----|-------------|
| Admin | 1 | Full access |
| User | 2 | Authenticated access |
| Guest | 3 | Limited access |
| Gamer | 4 | Game-specific access |
| Deleted | 5 | Soft-deleted user |
| Trusted | 6 | Extended access (formerly "Family") |

## File Management

- Local filesystem storage at `/data/files/`
- Per-user directories: `/data/files/{user_id}/`
- Shared directory: `/data/files/shared/`
- 50 GB quota per user, 50 GB for shared storage
- Supports folders, nested paths, upload/download/delete

## Game Scoring

- Word Weaver: Board submitted via POST, server finds longest valid words using locally loaded dictionary
- Generic game scores with JSONB metadata for flexible game-specific data
- Leaderboard queries with game-type filtering

## Service Proxying

The server acts as a reverse proxy to three microservices:

| Service | Default URL | Env Override | Purpose |
|---------|-------------|-------------|---------|
| Transcriber | `http://transcriber:8000` | `TRANSCRIBER_URL` | Voice-to-text |
| Commander | `http://commander:8000` | `COMMANDER_URL` | Voice command parsing |
| Translator | `http://translator:8000` | `TRANSLATOR_URL` | Language translation |

Proxy pattern: The server forwards HTTP requests and pipes responses back, preserving status codes and content types.
