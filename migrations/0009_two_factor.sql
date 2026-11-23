-- 两步验证（TOTP，RFC 6238）与账号安全。
-- totp_secret 存 AES-GCM 密文（AAD 绑定账号 id），空串表示未启用；明文只在设置页一次性和验证时短暂存在。
-- totp_last_step 记录已消费的时间步（unix/30），单调递增，防止同一动态码在登录与启用之间重复使用。
-- recovery_hashes 只存恢复码的 SHA-256；一次性消费后从数组移除。
-- auth_version 随密码、2FA 开关递增，旧会话与旧登录挑战随之失效。
alter table users
  add column if not exists totp_secret       text not null default '',
  add column if not exists totp_last_step    bigint not null default -1,
  add column if not exists recovery_hashes   text[] not null default '{}',
  add column if not exists auth_version      bigint not null default 0,
  add column if not exists otp_failures      integer not null default 0,
  add column if not exists otp_blocked_until timestamptz;
