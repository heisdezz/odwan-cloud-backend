export interface MediaItem {
  id: string;
  file_hash: string;
  storage_backend?: "telegram" | "s3" | "";
  storage_bucket?: string;
  storage_key?: string;
  storage_etag?: string;
  original_relative_path: string;
  current_relative_path: string;
  file_size: number;
  mime_type: string;
  duration_seconds: number | null;
  metadata_json: string | null;
  album_id: string | null;
  created_at: string;
  album_name?: string;
  upload_status: "pending" | "uploading" | "success" | "error";
  tags?: Tag[];
}

export interface Album {
  id: string;
  name: string;
  relative_path: string;
  description: string | null;
  media_count: number;
  created_at: string;
  cover_media_id?: string | null;
}

export interface Tag {
  id: string;
  name: string;
  color_hex: string;
  category: string;
  media_count?: number;
}

export interface MediaTag {
  media_id: string;
  tag_id: string;
}

export interface LibraryStats {
  total_items: number;
  images: number;
  videos: number;
  albums: number;
  tags: number;
  db_size_bytes: number;
  db_size_formatted: string;
  db_exists: boolean;
}

export interface ServerInfo {
  status: "online" | "offline" | "error";
  server: string;
  drive_name?: string;
  drive_path?: string;
  download_url?: string | null;
  server_ip?: string;
  server_port?: number;
  stats?: LibraryStats;
  error?: string;
}

export interface SyncProgress {
  bytesWritten: number;
  contentLength: number;
  percentage: number;
}

export type SyncStatus =
  | "idle"
  | "testing"
  | "connected"
  | "downloading"
  | "migrating"
  | "success"
  | "error";

export interface SavedServer {
  ip: string;
  port: number;
  driveName?: string;
  label?: string;
  lastConnectedAt?: string;
}

export interface ParsedMediaMetadata {
  width?: number;
  height?: number;
  codec?: string;
  bitrate?: number;
  fps?: number;
  camera_make?: string;
  camera_model?: string;
  date_taken?: string;
  iso?: number;
  exposure_time?: string;
  f_number?: number;
  latitude?: number;
  longitude?: number;
  [key: string]: any;
}
