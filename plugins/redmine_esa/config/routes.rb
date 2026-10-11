get 'esa', to: 'esa#index', as: :esa
get 'esa/upload', to: 'esa#upload', as: :esa_upload
post 'esa/preview', to: 'esa#preview', as: :esa_preview
post 'esa/import/:id', to: 'esa#import', as: :esa_import
get 'esa/tickets/:id', to: 'esa#show', as: :esa_ticket
post 'esa/tickets/:id/analyze', to: 'esa#analyze', as: :esa_analyze
post 'esa/tickets/:id/metadata', to: 'esa#metadata', as: :esa_metadata
post 'esa/tickets/:id/review_case', to: 'esa#review_case', as: :esa_review_case
post 'esa/tickets/:id/review_run', to: 'esa#review_run', as: :esa_review_run
get 'esa/api/events', to: 'esa_events#index'
post 'esa/api/events/ack', to: 'esa_events#ack'
