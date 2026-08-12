-- System category taxonomy (plan F0-01) and seed merchant aliases used by
-- the deterministic extraction path. IDs are fixed UUIDs for stable refs.

INSERT INTO categories (id, system_key, display_name, is_system) VALUES
    ('11111111-1111-1111-1111-111111111101', 'an-uong',      'Ăn uống',      true),
    ('11111111-1111-1111-1111-111111111102', 'thuc-pham',    'Thực phẩm',    true),
    ('11111111-1111-1111-1111-111111111103', 'di-lai',       'Đi lại',       true),
    ('11111111-1111-1111-1111-111111111104', 'hoa-don',      'Hóa đơn',      true),
    ('11111111-1111-1111-1111-111111111105', 'mua-sam',      'Mua sắm',      true),
    ('11111111-1111-1111-1111-111111111106', 'suc-khoe',     'Sức khỏe',     true),
    ('11111111-1111-1111-1111-111111111107', 'giai-tri',     'Giải trí',     true),
    ('11111111-1111-1111-1111-111111111108', 'giao-duc',     'Giáo dục',     true),
    ('11111111-1111-1111-1111-111111111109', 'nha-o',        'Nhà ở',        true),
    ('11111111-1111-1111-1111-111111111110', 'thu-nhap',     'Thu nhập',     true),
    ('11111111-1111-1111-1111-111111111111', 'hoan-tien',    'Hoàn tiền',    true),
    ('11111111-1111-1111-1111-111111111112', 'chuyen-khoan', 'Chuyển khoản', true),
    ('11111111-1111-1111-1111-111111111113', 'khac',         'Khác',         true);

-- Seed merchants give the categorisation engine sensible global defaults;
-- user_merchant_rules override these per user over time.
INSERT INTO merchants (id, canonical_name, normalised_name, merchant_family) VALUES
    ('22222222-2222-2222-2222-222222222201', 'Co.opmart',       'coopmart',      'supermarket'),
    ('22222222-2222-2222-2222-222222222202', 'Circle K',        'circle k',      'convenience'),
    ('22222222-2222-2222-2222-222222222203', 'Highlands Coffee','highlands coffee','cafe'),
    ('22222222-2222-2222-2222-222222222204', 'Grab',           'grab',           'transport'),
    ('22222222-2222-2222-2222-222222222205', 'Petrolimex',     'petrolimex',     'fuel'),
    ('22222222-2222-2222-2222-222222222206', 'Guardian',       'guardian',       'pharmacy'),
    ('22222222-2222-2222-2222-222222222207', 'Shopee',         'shopee',         'ecommerce'),
    ('22222222-2222-2222-2222-222222222208', 'The Coffee House','the coffee house','cafe');

INSERT INTO merchant_aliases (id, merchant_id, alias, normalised_alias, source) VALUES
    ('33333333-3333-3333-3333-333333333301', '22222222-2222-2222-2222-222222222201', 'CO.OPMART',          'coopmart',            'seed'),
    ('33333333-3333-3333-3333-333333333302', '22222222-2222-2222-2222-222222222201', 'Co.op Mart',         'coop mart',           'seed'),
    ('33333333-3333-3333-3333-333333333303', '22222222-2222-2222-2222-222222222203', 'HIGHLANDS COFFEE',   'highlands coffee',    'seed'),
    ('33333333-3333-3333-3333-333333333304', '22222222-2222-2222-2222-222222222203', 'Highlands',          'highlands',           'seed'),
    ('33333333-3333-3333-3333-333333333305', '22222222-2222-2222-2222-222222222204', 'GRAB',               'grab',                'seed'),
    ('33333333-3333-3333-3333-333333333306', '22222222-2222-2222-2222-222222222205', 'PETROLIMEX',         'petrolimex',          'seed'),
    ('33333333-3333-3333-3333-333333333307', '22222222-2222-2222-2222-222222222207', 'SHOPEE VN',          'shopee vn',           'seed'),
    ('33333333-3333-3333-3333-333333333308', '22222222-2222-2222-2222-222222222208', 'THE COFFEE HOUSE',   'the coffee house',    'seed');
