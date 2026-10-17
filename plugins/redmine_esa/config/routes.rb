get 'esa', to: 'esa#index', as: :esa
get 'esa/engineering_issues', to: 'esa#engineering_issues', as: :esa_engineering_issues
get 'esa/upload', to: 'esa#upload', as: :esa_upload
post 'esa/preview', to: 'esa#preview', as: :esa_preview
post 'esa/import/:id', to: 'esa#import', as: :esa_import
get 'esa/tickets/:id', to: 'esa#show', as: :esa_ticket
get 'esa/tickets/:id/status', to: 'esa#status', as: :esa_ticket_status
get 'esa/tickets/:id/report', to: 'esa#report', as: :esa_report
get 'esa/tickets/:id/metadata_suggestion', to: 'esa#metadata_suggestion', as: :esa_metadata_suggestion
post 'esa/tickets/:id/forward', to: 'esa#forward', as: :esa_forward
post 'esa/tickets/:id/metadata', to: 'esa#metadata', as: :esa_metadata
post 'esa/tickets/:id/review_case', to: 'esa#review_case', as: :esa_review_case
get 'esa/api/events', to: 'esa_events#index'
post 'esa/api/events/ack', to: 'esa_events#ack'

post 'esa/tickets/:id/classifier_predict', to: 'esa#classifier_predict', as: :esa_classifier_predict
post 'esa/tickets/:id/classifier_feedback', to: 'esa#classifier_feedback', as: :esa_classifier_feedback
post 'esa/tickets/:id/review_investigation', to: 'esa#review_investigation', as: :esa_review_investigation
post 'esa/tickets/:id/review_membership', to: 'esa#review_membership', as: :esa_review_membership
