export const config = {
  storage_path: "./storage",
  s3: {
    endpoint: "https://s3.eu-central-003.backblazeb2.com",
    region: "eu-central-003",
    bucket: "odw-cloud",
    part_size: 8 * 1024 * 1024,
    concurrency: 4,
  },
} as const;
