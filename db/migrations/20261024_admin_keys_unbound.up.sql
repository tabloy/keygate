-- An admin key reaches every product's catalogue, settings and keys, so
-- a product binding on it never held: it could mint an unbound key. The
-- dashboard also bound every admin key it created to the first product
-- without showing it. Admin keys are now always system-wide, and the
-- server refuses to create a bound one.
UPDATE api_keys SET product_id = NULL
 WHERE product_id IS NOT NULL AND 'admin' = ANY (scopes);
