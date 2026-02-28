-- Add "default" as the primary public template (Python 3 + Node.js 20 + build tools).
-- The old "base" template remains for backward compatibility but new sandboxes use "default".
INSERT INTO templates (org_id, name, tag, image_ref, is_public) VALUES
    (NULL, 'default', 'latest', 'opensandbox/default:latest', true)
ON CONFLICT DO NOTHING;

-- Also seed "ubuntu" as the bare-bones option (was previously only available as "base")
INSERT INTO templates (org_id, name, tag, image_ref, is_public) VALUES
    (NULL, 'ubuntu', 'latest', 'docker.io/library/ubuntu:22.04', true)
ON CONFLICT DO NOTHING;
