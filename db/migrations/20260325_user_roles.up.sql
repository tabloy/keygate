-- Add role column to users table to separate admins from regular users.
-- Roles: 'owner' (first admin, can manage other admins), 'admin', 'user' (customer/end-user)
-- This replaces the ADMIN_EMAILS env var approach with database-backed roles.
ALTER TABLE users ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'user'
    CHECK (role IN ('owner', 'admin', 'user'));

-- Create index for role lookups
CREATE INDEX IF NOT EXISTS idx_users_role ON users(role);
